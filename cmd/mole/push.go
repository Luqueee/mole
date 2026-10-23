// mole push — expose a local service on the remote (ssh -R) over the same
// SSH connection as the pull forwards.
//
//	mole push 3000[:8080]   add a push forward (applied live if a daemon runs)
//	mole unpush 3000        remove it
//	mole push list          show push forwards and their state
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/proxy"
)

// remoteListener opens listeners on the remote host; *tunnel.Manager does.
type remoteListener interface {
	Listen(network, addr string) (net.Listener, error)
}

// pushForwarder owns the remote listeners of push forwards. Remote
// listeners die with the SSH connection, so reopenAll must run after every
// reconnect (see tunnel.Manager.OnReconnect).
type pushForwarder struct {
	remote remoteListener
	dialer proxy.Dialer // reaches the local service
	log    *slog.Logger
	hooks  proxy.Hooks

	mu     sync.Mutex
	rules  map[int]config.PushRule // by local port
	active map[int]net.Listener
	failed map[int]string // bind error, logged once per failure streak
	// hidden rules belong to mole itself (the file-transfer receiver): they
	// are not listed and their traffic is not counted in the stats.
	hidden map[int]bool
}

func newPushForwarder(remote remoteListener, log *slog.Logger, hooks proxy.Hooks) *pushForwarder {
	return &pushForwarder{
		remote: remote,
		dialer: &net.Dialer{Timeout: 5 * time.Second},
		log:    log,
		hooks:  hooks,
		rules:  make(map[int]config.PushRule),
		active: make(map[int]net.Listener),
		failed: make(map[int]string),
		hidden: make(map[int]bool),
	}
}

// ensureLocked opens the remote listener for rule r if it isn't open.
func (f *pushForwarder) ensureLocked(r config.PushRule) error {
	if _, ok := f.active[r.Local]; ok {
		return nil
	}
	remoteAddr := net.JoinHostPort(r.BindAddr(), strconv.Itoa(r.RemotePort()))
	ln, err := f.remote.Listen("tcp", remoteAddr)
	if err != nil {
		err = fmt.Errorf("bind remote %s: %w (port in use on the remote, or AllowTcpForwarding disabled)", remoteAddr, err)
		if _, warned := f.failed[r.Local]; !warned {
			f.log.Warn("could not open push forward, skipping", "local", r.Local, "err", err)
		}
		f.failed[r.Local] = err.Error()
		return err
	}
	delete(f.failed, r.Local)
	f.active[r.Local] = ln
	localAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.Local))
	f.log.Info("pushing", "remote", remoteAddr, "local", localAddr)
	hooks := f.hooks
	if f.hidden[r.Local] {
		hooks = proxy.Hooks{Dir: proxy.DirPush}
	}
	go func() {
		if err := proxy.Serve(ln, f.dialer, localAddr, hooks.WithPort(r.Local), f.log); err != nil {
			f.log.Warn("push proxy terminated", "local", r.Local, "err", err)
		}
		f.mu.Lock()
		if f.active[r.Local] == ln {
			delete(f.active, r.Local)
		}
		f.mu.Unlock()
	}()
	return nil
}

// start registers rules from config. Failures stay registered (state
// "failed") and are retried after a reconnect.
func (f *pushForwarder) start(rules []config.PushRule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rules {
		f.rules[r.Local] = r
		_ = f.ensureLocked(r)
	}
}

// reopenAll drops every listener and opens them again on the (new) transport.
func (f *pushForwarder) reopenAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p, ln := range f.active {
		_ = ln.Close()
		delete(f.active, p)
	}
	for _, r := range f.rules {
		_ = f.ensureLocked(r)
	}
}

func (f *pushForwarder) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p, ln := range f.active {
		_ = ln.Close()
		delete(f.active, p)
	}
}

// AddPush implements admin.PushController. The rule is registered even if
// the remote bind fails (state "failed", error returned): like rules from
// the config, it is retried after every reconnect. A re-add replaces a
// failed or differently-configured rule.
func (f *pushForwarder) AddPush(local, remote int, bind string) error {
	r := config.PushRule{Local: local, Remote: remote, Bind: bind}
	if err := r.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.active[local]; ok {
		return fmt.Errorf("%w: local port %d is already pushed", admin.ErrConflict, local)
	}
	f.rules[local] = r
	return f.ensureLocked(r)
}

// addHidden opens an internal push forward (local -> same port on the remote)
// for mole's own use. Unlike AddPush it does not stay registered on failure:
// the caller decides whether to retry.
func (f *pushForwarder) addHidden(port, remotePort int) error {
	r := config.PushRule{Local: port}
	if remotePort != port {
		r.Remote = remotePort
	}
	if err := r.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[port]; ok {
		return fmt.Errorf("%w: local port %d is already pushed", admin.ErrConflict, port)
	}
	f.rules[port] = r
	f.hidden[port] = true
	if err := f.ensureLocked(r); err != nil {
		delete(f.rules, port)
		delete(f.hidden, port)
		delete(f.failed, port)
		return err
	}
	return nil
}

// removeHidden closes an internal push forward.
func (f *pushForwarder) removeHidden(port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hidden[port] {
		return
	}
	delete(f.hidden, port)
	delete(f.rules, port)
	delete(f.failed, port)
	if ln, ok := f.active[port]; ok {
		_ = ln.Close()
		delete(f.active, port)
	}
}

// RemovePush implements admin.PushController; idempotent.
func (f *pushForwarder) RemovePush(local int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hidden[local] {
		return nil // owned by mole itself, not a user push
	}
	delete(f.rules, local)
	delete(f.failed, local)
	if ln, ok := f.active[local]; ok {
		_ = ln.Close()
		delete(f.active, local)
		f.log.Info("unpushed (live remove)", "local", local)
	}
	return nil
}

// ListPush implements admin.PushController.
func (f *pushForwarder) ListPush() []admin.PushInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]admin.PushInfo, 0, len(f.rules))
	for _, r := range f.rules {
		if f.hidden[r.Local] {
			continue
		}
		info := admin.PushInfo{Local: r.Local, Remote: r.RemotePort(), Bind: r.BindAddr(), State: "active"}
		if _, ok := f.active[r.Local]; !ok {
			info.State = "failed"
			info.Error = f.failed[r.Local]
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Local < out[j].Local })
	return out
}

// parsePushSpec parses "3000" or "3000:8080".
func parsePushSpec(s string) (local, remote int, err error) {
	l, r, hasRemote := strings.Cut(s, ":")
	if local, err = strconv.Atoi(l); err != nil {
		return 0, 0, fmt.Errorf("invalid spec %q (expected LOCAL[:REMOTE])", s)
	}
	remote = local
	if hasRemote {
		if remote, err = strconv.Atoi(r); err != nil {
			return 0, 0, fmt.Errorf("invalid spec %q (expected LOCAL[:REMOTE])", s)
		}
	}
	if err = (config.PushRule{Local: local, Remote: remote}).Validate(); err != nil {
		return 0, 0, err
	}
	return local, remote, nil
}

func runPush(args []string) int {
	if len(args) > 0 && (args[0] == "list" || args[0] == "ls") {
		return runPushList(args[1:])
	}
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	bind := fs.String("bind", "", "address to bind on the remote (default 127.0.0.1)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole push LOCAL[:REMOTE] [flags]
       mole push list [flags]

Exposes the local port LOCAL on the remote at REMOTE (default: same port),
over the same SSH connection (ssh -R). Saved in mole.yaml under push: and
applied live if mole up is running.

Flags:
  -config  path to YAML config
  -bind    address to bind on the remote (default 127.0.0.1; anything else
           needs GatewayPorts on the remote sshd)`)
	}
	// Allow flags after the spec.
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	local, remote, err := parsePushSpec(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole push:", err)
		return 2
	}
	resolved, err := resolveOrCreateConfigPath(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %s: %v\n", resolved, err)
		return 1
	}
	rule := config.PushRule{Local: local, Bind: *bind}
	if remote != local {
		rule.Remote = remote
	}
	replaced := false
	for i, r := range cfg.Push {
		if r.Local == local {
			cfg.Push[i], replaced = rule, true
		}
	}
	if !replaced {
		cfg.Push = append(cfg.Push, rule)
	}
	if err := config.Save(resolved, cfg, "push"); err != nil {
		fmt.Fprintf(os.Stderr, "mole push: write %s: %v\n", resolved, err)
		return 1
	}
	fmt.Printf("push %d -> remote %s:%d saved in %s\n", local, rule.BindAddr(), remote, resolved)

	if cfg.AdminAddr == "" {
		return 0
	}
	if !daemonLive(cfg.AdminAddr) {
		fmt.Printf("note: mole up not running at %s; it will open the forward on start\n", cfg.AdminAddr)
		return 0
	}
	// Replace semantics for live apply: drop any previous forward first.
	_ = adminDo(http.MethodDelete, cfg.AdminAddr, fmt.Sprintf("/ports/push/%d", local), "", http.StatusOK)
	body, _ := json.Marshal(map[string]any{"local": local, "remote": remote, "bind": *bind})
	if err := adminDo(http.MethodPost, cfg.AdminAddr, "/ports/push", string(body), http.StatusCreated); err != nil {
		fmt.Fprintf(os.Stderr, "warning: live push failed (%v); saved, mole up retries it after the next reconnect\n", err)
		return 1
	}
	fmt.Printf("live: mole up is pushing %d now\n", local)
	return 0
}

func runUnpush(args []string) int {
	fs := flag.NewFlagSet("unpush", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: mole unpush LOCAL [-config path]\n\nRemoves the push forward for local port LOCAL and closes the remote listener.")
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	local, err := strconv.Atoi(fs.Arg(0))
	if err != nil || local < 1 || local > 65535 {
		fmt.Fprintf(os.Stderr, "mole unpush: invalid port %q\n", fs.Arg(0))
		return 2
	}
	resolved, err := resolveOrCreateConfigPath(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %s: %v\n", resolved, err)
		return 1
	}
	kept := cfg.Push[:0]
	for _, r := range cfg.Push {
		if r.Local != local {
			kept = append(kept, r)
		}
	}
	changed := len(kept) != len(cfg.Push)
	cfg.Push = kept
	if changed {
		if err := config.Save(resolved, cfg, "push"); err != nil {
			fmt.Fprintf(os.Stderr, "mole unpush: write %s: %v\n", resolved, err)
			return 1
		}
		fmt.Printf("removed push %d from %s\n", local, resolved)
	} else {
		fmt.Printf("%d is not in push: (no change)\n", local)
	}
	if cfg.AdminAddr != "" && daemonLive(cfg.AdminAddr) {
		if err := adminDo(http.MethodDelete, cfg.AdminAddr, fmt.Sprintf("/ports/push/%d", local), "", http.StatusOK); err != nil {
			fmt.Fprintf(os.Stderr, "warning: live unpush failed (%v); restart mole up to apply\n", err)
			return 1
		}
		fmt.Printf("live: mole up closed the remote listener for %d\n", local)
	}
	return 0
}

func runPushList(args []string) int {
	fs := flag.NewFlagSet("push list", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(resolveConfigPathReadOnly(*configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if cfg.AdminAddr != "" && daemonLive(cfg.AdminAddr) {
		if infos, err := fetchPushStatus(cfg.AdminAddr); err == nil {
			if len(infos) == 0 {
				fmt.Println("no push forwards")
			}
			for _, i := range infos {
				line := fmt.Sprintf("%d -> remote %s:%d  %s", i.Local, i.Bind, i.Remote, i.State)
				if i.Error != "" {
					line += ": " + i.Error
				}
				fmt.Println(line)
			}
			return 0
		}
	}
	if len(cfg.Push) == 0 {
		fmt.Println("no push forwards")
	}
	for _, r := range cfg.Push {
		fmt.Printf("%d -> remote %s:%d  (mole up not running)\n", r.Local, r.BindAddr(), r.RemotePort())
	}
	return 0
}

func resolveConfigPathReadOnly(path string) string {
	if path != "" {
		return path
	}
	return config.Find()
}

func fetchPushStatus(addr string) ([]admin.PushInfo, error) {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Info struct {
			Push []admin.PushInfo `json:"push"`
		} `json:"info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Info.Push, nil
}

// adminDo sends one request to the admin API and requires wantStatus.
func adminDo(method, addr, path, body string, wantStatus int) error {
	req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		return err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == wantStatus {
		return nil
	}
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return fmt.Errorf("admin %s: %s", resp.Status, strings.TrimSpace(string(preview)))
}

// reorderFlags moves non-flag args after flags so `mole push 3000 -bind x`
// works with the stdlib flag parser. Flags here all take a value.
func reorderFlags(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			rest = append(rest, a)
		}
	}
	return append(flags, rest...)
}
