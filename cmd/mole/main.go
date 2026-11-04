// Command mole is a smart local-port forwarder.
//
// It opens a single SSH connection to a remote machine and forwards
// one or more local TCP ports through it to the same port numbers on
// the remote. Optional auto-discover mode probes the remote for
// common dev ports and forwards the ones that respond.
//
// Usage:
//
//	mole up [flags]
//	mole status
//	mole version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/discover"
	"github.com/Luqueee/mole/internal/proxy"
	"github.com/Luqueee/mole/internal/tunnel"
)

const version = "0.9.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	// `mole -n NODE <command>` runs the command on another node.
	if node, rest, ok, err := splitNodeFlag(os.Args[1:]); ok {
		if err != nil {
			fmt.Fprintln(os.Stderr, "mole:", err)
			os.Exit(2)
		}
		os.Exit(runOnNode(node, rest))
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "up":
		os.Exit(runUp(args))
	case "down":
		os.Exit(runDown(args))
	case "status":
		os.Exit(runStatus(args))
	case "logs":
		os.Exit(runLogs(args))
	case "update":
		os.Exit(runUpdate(args))
	case "restart":
		os.Exit(runRestart(args))
	case "service":
		os.Exit(runService(args))
	case "clip":
		os.Exit(runClip(args))
	case "ports":
		os.Exit(runPorts(args))
	case "push":
		os.Exit(runPush(args))
	case "unpush":
		os.Exit(runUnpush(args))
	case "send", "ftp":
		os.Exit(runSend(args))
	case "get":
		os.Exit(runGet(args))
	case "inbox":
		os.Exit(runInbox(args))
	case "recv":
		os.Exit(runRecv(args))
	case "agent":
		os.Exit(runAgent(args))
	case "node":
		os.Exit(runNode(args))
	case "dns":
		os.Exit(runDNS(args))
	case "route":
		os.Exit(runRoute(args))
	case "domain":
		os.Exit(runDomain(args))
	case "doctor":
		os.Exit(runDoctor(args))
	case "config":
		os.Exit(runConfig(args))
	case "init":
		os.Exit(runInit(args))
	case "version":
		fmt.Println(versionLine(cliColor(os.Stdout)))
	case "help", "-h", "--help":
		printUsage()
	default:
		ec := cliColor(os.Stderr)
		fmt.Fprintf(os.Stderr, "%s unknown command: %s\n\n", cRed("✗", ec), cBold(cmd, ec))
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	color := cliColor(os.Stdout)

	fmt.Printf("\n%s\n\n", banner(version, color))

	fmt.Printf("    %s\n", commandLine("<command>", color))
	fmt.Println()

	fmt.Printf("  %s\n", cBold("COMMANDS", color))
	commands := [][2]string{
		{"up", "Start the forwarder (foreground, or -d for background)"},
		{"down", "Stop a backgrounded mole (started with 'up -d')"},
		{"status", "Query the local admin API"},
		{"logs", "Show the background daemon log (colourised; -f to follow)"},
		{"update", "Update mole in place to the latest release"},
		{"restart", "Stop and re-launch a backgrounded mole using the same config"},
		{"service", "Start mole at login and keep it running (service install|uninstall|status)"},
		{"ports", "Manage the auto-discover port list (add/remove/list)"},
		{"push", "Expose a local port on the remote (push list|LOCAL[:REMOTE])"},
		{"doctor", "Diagnose config, SSH, push and split DNS (doctor [name...])"},
		{"dns", "Route configured domains to the remote (dns install|uninstall; Linux)"},
		{"domain", "Set the domain named routes live under (domain set NAME|show)"},
		{"route", "Name a remote service: a.DOMAIN -> port (route add|rm|ls)"},
		{"unpush", "Remove a push forward"},
		{"send", "Send files to the other machine's inbox (picker when no files; alias: ftp)"},
		{"get", "Download files from another machine over SSH (resumes if interrupted)"},
		{"inbox", "Where received files land; accept incoming transfers (inbox -h)"},
		{"node", "Other machines you manage: node add|ls|rm|ping; run a command there with -n NODE"},
		{"init", "Generate a mole.yaml (interactive; run once per machine)"},
		{"config", "Open the active mole.yaml in $VISUAL / $EDITOR (see `mole config -h`)"},
		{"version", "Print version and exit"},
	}
	for _, c := range commands {
		fmt.Printf("    %s  %s\n", cGreen(fmt.Sprintf("%-8s", c[0]), color), c[1])
	}
	fmt.Println()

	hint := fmt.Sprintf("Run '%s' for up flags, '%s' for init flags.",
		commandLine("up -h", color), commandLine("init -h", color))
	fmt.Printf("  %s\n", cDim(hint, color))
	nodeHint := fmt.Sprintf("Run any command on another machine with '%s'.", commandLine("-n NODE <command>", color))
	fmt.Printf("  %s\n", cDim(nodeHint, color))
	fmt.Println()
}

// runUp runs `mole up`: the forwarder in the foreground (see upServer for
// its lifecycle), or with -d a detached copy of itself.
func runUp(args []string) int {
	flags, err := parseUpFlags(args, os.Stderr)
	if err != nil {
		// As with flag.ExitOnError: the flag package has printed why.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// Background mode: re-exec detached and return control to the shell.
	// The child carries MOLE_DAEMONIZED=1 and falls through to run the
	// forwarder in the foreground of its own session.
	if flags.detached() && os.Getenv("MOLE_DAEMONIZED") == "" {
		return daemonize(stripDetachFlags(args))
	}
	// SIGINT/SIGTERM (and `mole down` on Windows) stop the run at any stage,
	// including while the first connection waits for the network.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	stopCtx, stopRun := stopContext(sigCtx, stopSignals)
	defer stopRun()
	return newUpServer(flags).run(stopCtx)
}

// Localhost names already resolve to loopback in browsers. They need only the
// unprivileged route listener, not the OS-specific split-DNS address.
func needsDNSStub(domains []string) bool {
	for _, domain := range domains {
		if domain != "localhost" && !strings.HasSuffix(domain, ".localhost") {
			return true
		}
	}
	return false
}

// loopbackPort returns the port of a listen address that occupies the
// loopback port forwards bind (127.0.0.1:9999, localhost:9999, 0.0.0.0:9999).
func loopbackPort(addr string) (int, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !(ip.IsLoopback() || ip.IsUnspecified())) {
		return 0, false
	}
	p, err := strconv.Atoi(port)
	return p, err == nil && p > 0 && p <= 65535
}

// forwarder owns the set of active local listeners, one per forwarded
// port, and can add new ones on the fly. Safe for concurrent use so the
// periodic auto-discovery goroutine and startup path can both call
// ensure without racing.
type forwarder struct {
	mgr         *tunnel.Manager
	remoteLabel string
	log         *slog.Logger
	hooks       proxy.Hooks

	mu     sync.Mutex
	active map[int]net.Listener
	failed map[int]bool // ports whose bind failed (warned once already)
	pinned map[int]bool // explicitly-configured ports; never auto-pruned
	closed bool         // closeAll ran: no new listeners

	// exclude is consulted by AddDiscover (the live-add path) so
	// the admin API returns 409 Conflict for excluded ports
	// instead of starting a listener that auto-discover would
	// immediately prune. Set once at construction; the same set
	// of excluded ports is enforced everywhere.
	exclude map[int]bool

	// loops, when set, says a port must not be pulled because a push forward
	// of the same number would loop with it.
	loops func(port int) bool
}

// ensure starts forwarding port p if it isn't already. Binding failures
// (e.g. the local port is taken by another process) are logged once and
// then retried silently — auto-discovery calls this every 15s, so
// without the dedupe a permanently-taken port would spam the log
// forever. A later success clears the warned state. The error is returned
// so live callers can distinguish a successful add from a failed bind.
func (f *forwarder) ensure(p int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensureLocked(p)
}

// ensureLocked is the lock-held implementation of ensure. It lets live
// mutation check and create a listener as one atomic operation.
func (f *forwarder) ensureLocked(p int) error {
	if f.closed {
		return errForwarderClosed
	}
	if _, ok := f.active[p]; ok {
		return nil
	}
	if f.loops != nil && f.loops(p) {
		if !f.failed[p] {
			f.log.Warn("not forwarding a port that is also pushed to the same number (it would loop)", "port", p)
			f.failed[p] = true
		}
		return fmt.Errorf("port %d is pushed to the same port on the remote: forwarding it back would loop", p)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		if !f.failed[p] {
			f.log.Warn("could not bind local port, skipping", "port", p, "err", err)
			f.failed[p] = true
		}
		return fmt.Errorf("bind local port %d: %w", p, err)
	}
	delete(f.failed, p)
	remoteAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
	f.active[p] = ln
	f.log.Info("forwarding", "local", ln.Addr().String(), "remote", net.JoinHostPort(f.remoteLabel, strconv.Itoa(p)))
	hooks := f.hooks.WithPort(p)
	go func(p int, listener net.Listener) {
		if err := proxy.Serve(listener, f.mgr, remoteAddr, hooks, f.log); err != nil {
			f.log.Warn("proxy terminated", "port", p, "err", err)
		}
		f.mu.Lock()
		if current, ok := f.active[p]; ok && current == listener {
			delete(f.active, p)
		}
		f.mu.Unlock()
	}(p, ln)
	return nil
}

// ports returns the sorted list of currently forwarded ports.
func (f *forwarder) ports() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, 0, len(f.active))
	for p := range f.active {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// retain prunes auto-discovered listeners that are no longer present on
// the remote. keep is the set of ports currently listening there; any
// active port not in keep — and not pinned (explicitly configured) — is
// closed and removed, so a dead remote service stops being advertised by
// `mole status` and stops accepting local connections that can only fail.
// When the same port comes back, ensure() forwards it afresh and logs it
// again. Pinned ports are never pruned: the user asked for them by name.
func (f *forwarder) retain(keep map[int]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p, ln := range f.active {
		if keep[p] || f.pinned[p] {
			continue
		}
		_ = ln.Close()
		delete(f.active, p)
		f.log.Info("unforwarding", "port", p, "remote", net.JoinHostPort(f.remoteLabel, strconv.Itoa(p)))
	}
}

// errForwarderClosed is returned by ensure after closeAll.
var errForwarderClosed = errors.New("the forwarder is shut down")

// closeAll shuts down every active listener. The forwarder opens none
// afterwards, so a discovery sweep still running at shutdown cannot leave a
// listener behind.
func (f *forwarder) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for p, ln := range f.active {
		_ = ln.Close()
		delete(f.active, p)
	}
}

// AddDiscover starts forwarding port as if it had been on the
// discover_ports: list at startup. Thread-safe; idempotent.
// Returns a non-nil error if the port is in the excluded set or
// the local bind fails — the admin API maps both to HTTP 409.
//
// Note: the underlying ensure() does not know that the port was
// added "live", so a subsequent retain() (auto-discover pruning)
// may close the listener if the remote isn't actually serving on
// that port. That's correct behaviour: the user added the port,
// but the remote service isn't there. They'll see "unforwarding"
// in the log and can either start the service or remove the
// port. The pinned set, not this method, is the right tool for
// "always forward regardless of what the remote is doing".
func (f *forwarder) AddDiscover(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port out of range: %d", port)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exclude[port] {
		return fmt.Errorf("%w: port %d is in exclude_ports", admin.ErrConflict, port)
	}
	if _, ok := f.active[port]; ok {
		return fmt.Errorf("%w: port %d is already forwarded", admin.ErrConflict, port)
	}
	return f.ensureLocked(port)
}

// RemoveDiscover stops forwarding port if it was started via
// AddDiscover (or, equivalently, was in the on-disk discover list
// at startup). Thread-safe; idempotent — a port that isn't active
// is a no-op, not an error.
func (f *forwarder) RemoveDiscover(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port out of range: %d", port)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ln, ok := f.active[port]
	if !ok {
		return nil // not forwarded; nothing to do
	}
	_ = ln.Close()
	delete(f.active, port)
	delete(f.failed, port)
	f.log.Info("unforwarded (live remove)", "port", port, "remote", net.JoinHostPort(f.remoteLabel, strconv.Itoa(port)))
	return nil
}

// discoverInto reconciles the forwarded port set with what's live on the
// remote. It prefers enumerating the remote's actual TCP listeners (so
// any port is found); when that enumeration is authoritative it also
// prunes ports that have since died (see forwarder.retain). It falls back
// to probing the fixed candidate list — add-only, no pruning — when
// ss/netstat aren't available or print nothing it can read, since a probe
// can't prove a port is gone.
//
// inRange is discover_range: an enumerated listener outside it is not
// forwarded on its own. It never applies to ports the user named — the
// candidates (discover_ports, which `mole ports add` writes) and pinned
// ports — nor to the probe fallback, which only probes candidates.
func discoverInto(ctx context.Context, fwd *forwarder, mgr *tunnel.Manager, candidates []int, exclude map[int]bool, inRange func(port int) bool, log *slog.Logger) {
	found, authoritative := discover.RemoteListeners(mgr, log)
	if !authoritative {
		// Couldn't enumerate (tool missing, unreadable output, or
		// transport down). Probe the candidate list and only add — never
		// prune on a guess.
		probed, err := discover.ProbeWithFactory(ctx, func(probeCtx context.Context) (discover.SweepDialer, error) {
			return mgr.NewProbeDialer(probeCtx)
		}, candidates, log)
		if err != nil {
			log.Debug("candidate probing failed", "err", err)
			return
		}
		for _, p := range probed {
			if !exclude[p] {
				fwd.ensure(p)
			}
		}
		return
	}
	named := make(map[int]bool, len(candidates))
	for _, p := range candidates {
		named[p] = true
	}
	forward, keep, skipped := planListeners(found, exclude, func(p int) bool {
		if inRange(p) || named[p] {
			return true
		}
		fwd.mu.Lock()
		defer fwd.mu.Unlock()
		return fwd.pinned[p]
	})
	if len(skipped) > 0 {
		log.Debug("listeners outside discover_range not forwarded", "ports", skipped)
	}
	for _, p := range forward {
		fwd.ensure(p)
	}
	fwd.retain(keep)
}

// planListeners turns an authoritative listener enumeration into the ports
// to forward and the set forwarder.retain must keep. Excluded ports are
// neither. A listener is forwarded when eligible(p); one that is not
// (outside discover_range) is reported in skipped but still kept, so a
// port already forwarded at the user's request — added live with `mole
// ports add` — is not pruned while it keeps listening.
func planListeners(found []int, exclude map[int]bool, eligible func(port int) bool) (forward []int, keep map[int]bool, skipped []int) {
	keep = make(map[int]bool, len(found))
	for _, p := range found {
		if exclude[p] {
			continue
		}
		keep[p] = true
		if eligible(p) {
			forward = append(forward, p)
		} else {
			skipped = append(skipped, p)
		}
	}
	return forward, keep, skipped
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addrFlag := fs.String("admin", "", "admin API address (default: admin_addr from the config)")
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Without -admin, use the address mole up listens on: admin_addr from the
	// same config it reads.
	addr := *addrFlag
	if addr == "" {
		path := resolveConfigPathReadOnly(*configPath)
		cfg, err := config.Load(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			return 1
		}
		if cfg.AdminAddr == "" {
			where := "no config file was found"
			if path != "" {
				where = "admin_addr is not set in " + path
			}
			fmt.Fprintf(os.Stderr, "mole status: the admin API is disabled (%s).\n", where)
			fmt.Fprintln(os.Stderr, "Set admin_addr: 127.0.0.1:9999 in the config and restart mole, or pass -admin ADDR if mole up was started with -admin.")
			return 1
		}
		addr = cfg.AdminAddr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/status")
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not reach admin API at", addr, ":", err)
		fmt.Fprintln(os.Stderr, "is mole running?")
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "admin returned", resp.Status)
		return 1
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read response:", err)
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger { return newLoggerTo(os.Stderr, level) }

// newLoggerTo returns a text logger writing to w at level.
func newLoggerTo(w io.Writer, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
