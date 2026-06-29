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

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
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
	case "init":
		os.Exit(runInit(args))
	case "version", "-v", "--version":
		fmt.Println("mole", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`mole — local-port forwarder with auto-discover

Usage:
  mole up [flags]
  mole down
  mole status [flags]
  mole logs [flags]
  mole init [flags]
  mole version
  mole help

Commands:
  up       Start the forwarder (foreground, or -d for background)
  down     Stop a backgrounded mole (started with 'up -d')
  status   Query the local admin API
  logs     Show the background daemon log (colourised; -f to follow)
  init     Generate a mole.yaml interactively (or via flags)
  version  Print version and exit

Run 'mole up -h' for up flags, 'mole init -h' for init flags.`)
}

func runUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	var (
		configPath = fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
		remote     = fs.String("remote", "", "SSH target, e.g. dev@workstation[:port]")
		ports      = fs.String("ports", "", "comma-separated ports to forward")
		autoDisc   = fs.Bool("auto-discover", false, "probe remote for common dev ports")
		adminAddr  = fs.String("admin", "", "admin HTTP address (default 127.0.0.1:9999; empty to disable)")
		logLevel   = fs.String("log-level", "", "debug|info|warn|error")
		detach     = fs.Bool("d", false, "run in the background (daemon)")
		detachLong = fs.Bool("detach", false, "alias for -d")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole up [flags]

Flags:
  -config         path to YAML config (default: ./mole.yaml, then user-global)
  -remote         SSH target, e.g. dev@workstation[:port]
  -ports          comma-separated ports to forward (e.g. 3000,5173)
  -auto-discover  probe remote for common dev ports
  -admin          admin HTTP address (empty to disable)
  -log-level      debug|info|warn|error
  -d, -detach     run in the background (daemon); stop with 'mole down'

Either -remote or a config file with 'remote:' is required.`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Background mode: re-exec detached and return control to the shell.
	// The child carries MOLE_DAEMONIZED=1 and falls through to run the
	// forwarder in the foreground of its own session.
	if (*detach || *detachLong) && os.Getenv("MOLE_DAEMONIZED") == "" {
		return daemonize(stripDetachFlags(args))
	}
	// When we are the detached child, clean up the pidfile on exit.
	if os.Getenv("MOLE_DAEMONIZED") != "" {
		defer os.Remove(pidPath())
	}

	// When no explicit -config is given, search the standard locations
	// (project-local ./mole.yaml, then user-global) so a config written
	// by `mole init -global` is picked up automatically.
	resolvedPath := *configPath
	if resolvedPath == "" {
		resolvedPath = config.Find()
	}
	cfg, err := config.Load(resolvedPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}

	// CLI overrides.
	if *remote != "" {
		cfg.Remote = *remote
	}
	if *ports != "" {
		cfg.Ports = config.ParsePorts(*ports)
	}
	if *autoDisc {
		cfg.AutoDiscover = true
	}
	if *adminAddr != "" {
		cfg.AdminAddr = *adminAddr
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}

	if cfg.Remote == "" {
		fmt.Fprintln(os.Stderr, "error: -remote is required (or set 'remote:' in config)")
		return 1
	}

	log := newLogger(cfg.LogLevel)

	rem, err := tunnel.ResolveRemote(cfg.Remote, cfg.SSHPort)
	if err != nil {
		log.Error("invalid remote", "err", err)
		return 1
	}

	log.Info("connecting to remote", "remote", cfg.Remote, "ssh_addr", rem.Addr, "user", rem.User)
	mgr, err := tunnel.New(rem, log)
	if err != nil {
		log.Error("ssh connect failed", "err", err)
		return 1
	}
	defer mgr.Close()

	stats := admin.NewStats()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Watch SSH connection and auto-reconnect.
	go mgr.Watch(ctx, 10*time.Second)

	// The forwarder owns the per-port listeners and can add new ones at
	// runtime, which is what makes periodic auto-discovery work.
	fwd := &forwarder{
		active:      make(map[int]net.Listener),
		failed:      make(map[int]bool),
		mgr:         mgr,
		remoteLabel: cfg.Remote,
		log:         log,
		hooks: proxy.Hooks{
			OnConnect:    stats.OnConnect,
			OnDisconnect: stats.OnDisconnect,
			OnDialFail:   stats.OnDialFail,
		},
	}
	defer fwd.closeAll()

	// Forward any explicitly-configured ports up front.
	for _, p := range cfg.Ports {
		fwd.ensure(p)
	}

	if cfg.AutoDiscover {
		exclude := make(map[int]bool, len(cfg.ExcludePorts))
		for _, p := range cfg.ExcludePorts {
			exclude[p] = true
		}
		// Initial sweep, then keep re-discovering so dev servers that
		// come up after launch are forwarded automatically — no need to
		// restart mole when you start your dev server.
		discoverInto(fwd, mgr, cfg.DiscoverPorts, exclude, log)
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					discoverInto(fwd, mgr, cfg.DiscoverPorts, exclude, log)
				}
			}
		}()
	}

	// Without auto-discover, an empty port set is a usage error. With
	// it, an empty set just means "nothing up yet" — keep watching.
	if !cfg.AutoDiscover && len(fwd.ports()) == 0 {
		log.Error("no ports to forward — use -ports or -auto-discover")
		return 1
	}
	if len(fwd.ports()) == 0 {
		log.Info("no dev ports up on the remote yet — watching; start a server there and mole will forward it", "recheck", "15s")
	}

	// Admin HTTP API.
	var adminSrv *http.Server
	if cfg.AdminAddr != "" {
		info := map[string]any{
			"remote":        cfg.Remote,
			"auto_discover": cfg.AutoDiscover,
		}
		srv := admin.New(stats, info).WithPorts(fwd.ports)
		adminSrv = &http.Server{
			Addr:              cfg.AdminAddr,
			Handler:           srv.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Info("admin listening", "addr", cfg.AdminAddr)
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("admin server stopped", "err", err)
			}
		}()
	}

	log.Info("mole up. Press Ctrl+C to stop.")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down")

	cancel()
	if adminSrv != nil {
		shutdownCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = adminSrv.Shutdown(shutdownCtx)
	}
	return 0
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
}

// ensure starts forwarding port p if it isn't already. Binding failures
// (e.g. the local port is taken by another process) are logged once and
// then retried silently — auto-discovery calls this every 15s, so
// without the dedupe a permanently-taken port would spam the log
// forever. A later success clears the warned state.
func (f *forwarder) ensure(p int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.active[p]; ok {
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		if !f.failed[p] {
			f.log.Warn("could not bind local port, skipping", "port", p, "err", err)
			f.failed[p] = true
		}
		return
	}
	delete(f.failed, p)
	f.active[p] = ln
	remoteAddr := fmt.Sprintf("127.0.0.1:%d", p)
	f.log.Info("forwarding", "local", ln.Addr().String(), "remote", fmt.Sprintf("%s:%d", f.remoteLabel, p))
	go func() {
		if err := proxy.Serve(ln, f.mgr, remoteAddr, f.hooks, f.log); err != nil {
			f.log.Warn("proxy terminated", "port", p, "err", err)
		}
	}()
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

// closeAll shuts down every active listener.
func (f *forwarder) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ln := range f.active {
		_ = ln.Close()
	}
}

// discoverInto finds ports to forward on the remote and forwards any
// new ones via fwd. It prefers enumerating the remote's actual TCP
// listeners (so any port is found), falling back to probing the fixed
// candidate list when ss/netstat aren't available on the remote.
func discoverInto(fwd *forwarder, mgr *tunnel.Manager, candidates []int, exclude map[int]bool, log *slog.Logger) {
	found := discover.RemoteListeners(mgr, log)
	if len(found) == 0 {
		found = discover.Probe(mgr, candidates, log)
	}
	for _, p := range found {
		if exclude[p] {
			continue
		}
		fwd.ensure(p)
	}
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("admin", "127.0.0.1:9999", "admin API address")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resp, err := http.Get("http://" + *addr + "/status")
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not reach admin API at", *addr, ":", err)
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

func newLogger(level string) *slog.Logger {
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
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
