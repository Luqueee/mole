// mole doctor — diagnose why something isn't working: config, SSH (auth,
// forwarding), the daemon, push forwards and the split-DNS setup, one
// verdict per line with the fix next to it.
//
//	mole doctor                  check everything the config enables
//	mole doctor admin.kena.lan   …and probe that name end to end
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/meshdns"
	"github.com/Luqueee/mole/internal/tunnel"
	"golang.org/x/crypto/ssh"
)

type level int

const (
	lvOK level = iota
	lvInfo
	lvWarn
	lvFail
)

// result is one check outcome. hint is the concrete fix, shown under it.
type result struct {
	lv     level
	title  string
	detail string
	hint   string
}

func ok(title, detail string) result   { return result{lvOK, title, detail, ""} }
func info(title, detail string) result { return result{lvInfo, title, detail, ""} }
func warn(title, detail, hint string) result {
	return result{lvWarn, title, detail, hint}
}
func fail(title, detail, hint string) result {
	return result{lvFail, title, detail, hint}
}

// reporter prints results as they happen and tallies them.
type reporter struct {
	w      io.Writer
	color  bool
	counts [4]int
	start  time.Time
}

func newReporter(w io.Writer, color bool) *reporter {
	return &reporter{w: w, color: color, start: time.Now()}
}

func (r *reporter) section(name string) {
	fmt.Fprintf(r.w, "\n  %s %s\n", cMagenta("❯", r.color), cBold(name, r.color))
}

func (r *reporter) add(res result) {
	r.counts[res.lv]++
	var icon string
	switch res.lv {
	case lvOK:
		icon = cGreen("✓", r.color)
	case lvInfo:
		icon = cDim("○", r.color)
	case lvWarn:
		icon = cYellow("⚠", r.color)
	case lvFail:
		icon = cRed("✗", r.color)
	}
	title := fmt.Sprintf("%-20s", res.title)
	if res.lv == lvFail {
		title = cRed(title, r.color)
	} else if res.lv == lvInfo {
		title = cDim(title, r.color)
	}
	fmt.Fprintf(r.w, "    %s  %s %s\n", icon, title, cGray(res.detail, r.color))
	if res.hint != "" {
		fmt.Fprintf(r.w, "       %s %s\n", cCyan("↳", r.color), cBold(res.hint, r.color))
	}
}

// summary prints the totals and returns the process exit code.
func (r *reporter) summary() int {
	fmt.Fprintf(r.w, "\n  %s\n", separator(r.color))
	parts := []string{
		cGreen(fmt.Sprintf("✓ %d ok", r.counts[lvOK]), r.color),
	}
	if n := r.counts[lvWarn]; n > 0 {
		parts = append(parts, cYellow(fmt.Sprintf("⚠ %d warning", n)+plural(n), r.color))
	}
	if n := r.counts[lvFail]; n > 0 {
		parts = append(parts, cRed(fmt.Sprintf("✗ %d failed", n), r.color))
	}
	if n := r.counts[lvInfo]; n > 0 {
		parts = append(parts, cDim(fmt.Sprintf("○ %d skipped", n), r.color))
	}
	fmt.Fprintf(r.w, "  %s  %s\n\n", strings.Join(parts, "   "),
		cDim("· "+time.Since(r.start).Round(time.Millisecond).String(), r.color))
	if r.counts[lvFail] > 0 {
		return 1
	}
	return 0
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// sshProbe is the part of *tunnel.Manager the SSH checks use.
type sshProbe interface {
	Listen(network, addr string) (net.Listener, error)
	Dial(network, addr string) (net.Conn, error)
	Run(cmd string) ([]byte, error)
}

var errProbeTimeout = errors.New("no answer from the SSH session")

// boundedProbe puts a deadline on every SSH call made after login. The
// underlying calls (NewSession, Dial, global requests) block indefinitely on
// a server that authenticated but stopped answering, so each runs in a
// goroutine; on timeout the connection is aborted (which releases it), the
// probe is marked dead and every later call fails immediately (the Manager
// would otherwise redial the same stalled server and wait again).
// shortcut: abandoned goroutines are released by abort, fine for a one-shot CLI.
type boundedProbe struct {
	p     sshProbe
	d     time.Duration
	abort func()
	dead  *atomic.Bool
}

func bounded[T any](b boundedProbe, what string, fn func() (T, error)) (T, error) {
	var zero T
	if b.dead.Load() {
		return zero, fmt.Errorf("%s: %w", what, errProbeTimeout)
	}
	type out struct {
		v   T
		err error
	}
	ch := make(chan out, 1)
	go func() { v, err := fn(); ch <- out{v, err} }()
	select {
	case o := <-ch:
		return o.v, o.err
	case <-time.After(b.d):
		b.dead.Store(true)
		b.abort()
		return zero, fmt.Errorf("%s: %w after %s", what, errProbeTimeout, b.d)
	}
}

func (b boundedProbe) Dial(network, addr string) (net.Conn, error) {
	return bounded(b, "open channel", func() (net.Conn, error) { return b.p.Dial(network, addr) })
}

func (b boundedProbe) Listen(network, addr string) (net.Listener, error) {
	ln, err := bounded(b, "remote listen", func() (net.Listener, error) { return b.p.Listen(network, addr) })
	if err != nil {
		return nil, err
	}
	return boundedListener{Listener: ln, b: b}, nil
}

// boundedListener bounds Close: x/crypto sends cancel-tcpip-forward and waits
// for the reply, which a misbehaving server may never send.
type boundedListener struct {
	net.Listener
	b boundedProbe
}

func (l boundedListener) Close() error {
	_, err := bounded(l.b, "cancel remote listener", func() (struct{}, error) { return struct{}{}, l.Listener.Close() })
	return err
}

func (b boundedProbe) Run(cmd string) ([]byte, error) {
	return bounded(b, "remote command", func() ([]byte, error) { return b.p.Run(cmd) })
}

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	timeout := fs.Duration("timeout", 10*time.Second, "SSH connect timeout")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole doctor [-config PATH] [-timeout 10s] [name...]

Checks the config, the SSH connection, push forwards and the split-DNS setup
and prints a verdict with the fix for each problem. Names (e.g. admin.kena.lan)
are probed end to end: remote DNS, local resolution, TCP connect.
Exit status is 1 if any check failed.`)
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	color := cliColor(os.Stdout)
	rep := newReporter(os.Stdout, color)
	fmt.Printf("\n  %s %s\n", cBold(cMagenta("mole doctor", color), color), cDim("v"+version, color))

	rep.section("Config")
	cfg, rem, good := doctorConfig(rep, *configPath)
	if !good {
		return rep.summary()
	}

	rep.section("SSH")
	var probe sshProbe
	if m := doctorSSH(rep, cfg, rem, *timeout); m != nil {
		defer m.Close()
		probe = boundedProbe{p: m, d: *timeout, abort: func() { _ = m.Close() }, dead: new(atomic.Bool)}
		doctorForwarding(rep, probe, rem, len(cfg.Push) > 0)
	}

	rep.section("Daemon")
	live := doctorDaemon(rep, cfg)

	if len(cfg.Push) > 0 {
		rep.section("Push")
		doctorPush(rep, cfg, live)
	}
	if len(cfg.Domains) > 0 || fs.NArg() > 0 {
		rep.section("Split DNS")
		doctorDomains(rep, cfg, live)
		for _, name := range fs.Args() {
			doctorName(rep, cfg, probe, name)
		}
	}
	return rep.summary()
}

func doctorConfig(rep *reporter, explicit string) (*config.Config, tunnel.Remote, bool) {
	path := explicit
	if path == "" {
		path = config.Find()
	}
	cfg, err := config.Load(path)
	if err != nil {
		rep.add(fail("config", err.Error(), "fix the YAML (see `mole config edit`)"))
		return nil, tunnel.Remote{}, false
	}
	if path == "" {
		rep.add(warn("config", "none found — using defaults", "run `mole init` to create one"))
	} else {
		rep.add(ok("config", path))
	}
	if cfg.Remote == "" {
		rep.add(fail("remote", "not set", "set `remote:` in the config or run `mole init`"))
		return cfg, tunnel.Remote{}, false
	}
	rem, err := tunnel.ResolveRemote(cfg.Remote, cfg.SSHPort)
	if err != nil {
		rep.add(fail("remote", cfg.Remote+": "+err.Error(), "check the host alias with `ssh -G "+cfg.Remote+"`"))
		return cfg, rem, false
	}
	via := ""
	if n := len(rem.ProxyJumps); n > 0 {
		via = fmt.Sprintf(" via %d jump host%s", n, plural(n))
	}
	rep.add(ok("remote", fmt.Sprintf("%s → %s as %s%s", cfg.Remote, rem.Addr, rem.User, via)))
	return cfg, rem, true
}

// doctorSSH connects and authenticates; it returns nil (after reporting why)
// when it can't.
func doctorSSH(rep *reporter, cfg *config.Config, rem tunnel.Remote, timeout time.Duration) *tunnel.Manager {
	type res struct {
		m   *tunnel.Manager
		err error
	}
	ch := make(chan res, 1)
	t0 := time.Now()
	go func() {
		m, err := tunnel.New(rem, cfg.Insecure, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ch <- res{m, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			title, hint := classifySSHError(r.err, cfg.Remote)
			rep.add(fail("connect", title, hint))
			return nil
		}
		rep.add(ok("connect", fmt.Sprintf("authenticated in %s", time.Since(t0).Round(time.Millisecond))))
		return r.m
	case <-time.After(timeout):
		rep.add(fail("connect", fmt.Sprintf("no answer after %s", timeout), "check the network/VPN, jump hosts and `ssh -v "+cfg.Remote+"`"))
		return nil
	}
}

// classifySSHError turns an SSH setup error into a short cause and a fix.
func classifySSHError(err error, remote string) (string, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no SSH auth methods"):
		return "no usable credentials", "start ssh-agent and `ssh-add`, or put a key in ~/.ssh/id_ed25519"
	case strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "no supported methods"):
		return "the server rejected our keys", "add your public key to the remote's authorized_keys (`ssh-copy-id " + remote + "`)"
	case strings.Contains(msg, "host key") || strings.Contains(msg, "known_hosts"):
		return "host key verification failed", "check ~/.ssh/known_hosts (or --insecure for a throwaway host)"
	case strings.Contains(msg, "refused"):
		return "connection refused", "is sshd running and the port right? (`ssh_port:` in the config)"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "no route"), strings.Contains(msg, "unreachable"):
		return "host unreachable", "check the network/VPN and any ProxyJump host"
	}
	return msg, "run `ssh -v " + remote + "` to see where it stops"
}

// doctorForwarding checks that sshd allows pull (direct-tcpip) and push
// (tcpip-forward) channels.
func doctorForwarding(rep *reporter, p sshProbe, rem tunnel.Remote, pushWanted bool) {
	_, port, err := net.SplitHostPort(rem.Addr)
	if err != nil {
		port = "22"
	}
	conn, err := p.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	var openErr *ssh.OpenChannelError
	switch {
	case err == nil:
		_ = conn.Close()
		rep.add(ok("pull forwarding", "direct-tcpip channels open"))
	case errors.As(err, &openErr) && openErr.Reason == ssh.Prohibited:
		rep.add(fail("pull forwarding", "sshd refuses channels (administratively prohibited)", "set `AllowTcpForwarding yes` in the remote sshd_config"))
	case errors.As(err, &openErr):
		// the channel opened and the target refused: forwarding itself is allowed
		rep.add(ok("pull forwarding", "direct-tcpip channels open"))
	default:
		if errors.Is(err, errProbeTimeout) {
			rep.add(fail("pull forwarding", err.Error(), "sshd accepted the login but doesn't answer channel requests; check the remote and `ssh -v`"))
		} else {
			rep.add(warn("pull forwarding", err.Error(), ""))
		}
	}

	ln, err := p.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		if cerr := ln.Close(); errors.Is(cerr, errProbeTimeout) {
			rep.add(warn("push forwarding", "tcpip-forward accepted, but the remote never answered its cancellation", "the remote leaves the probe port open until the session ends; check the remote sshd"))
			return
		}
		rep.add(ok("push forwarding", "tcpip-forward accepted"))
		return
	}
	if errors.Is(err, errProbeTimeout) {
		rep.add(fail("push forwarding", err.Error(), "the SSH session stopped answering; check the remote and `ssh -v`"))
		return
	}
	detail := "sshd refused the remote listener: " + err.Error()
	hint := "set `AllowTcpForwarding yes` (and not `no`/`local`) in the remote sshd_config"
	if pushWanted {
		rep.add(fail("push forwarding", detail, hint))
	} else {
		rep.add(warn("push forwarding", detail+" (push would not work)", hint))
	}
}

func doctorDaemon(rep *reporter, cfg *config.Config) bool {
	if cfg.AdminAddr == "" {
		rep.add(info("admin API", "disabled (admin_addr is empty)"))
		return false
	}
	if !daemonLive(cfg.AdminAddr) {
		rep.add(info("mole up", "not running at "+cfg.AdminAddr))
		for _, p := range cfg.Ports {
			if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p))); err != nil {
				rep.add(warn(fmt.Sprintf("local port %d", p), "already in use by another process", "free it or change `ports:`; mole up would skip it"))
			} else {
				_ = ln.Close()
			}
		}
		return false
	}
	rep.add(ok("mole up", "running at "+cfg.AdminAddr))
	return true
}

func doctorPush(rep *reporter, cfg *config.Config, live bool) {
	states := map[int]string{}
	errs := map[int]string{}
	if live {
		if infos, err := fetchPushStatus(cfg.AdminAddr); err == nil {
			for _, i := range infos {
				states[i.Local], errs[i.Local] = i.State, i.Error
			}
		}
	}
	for _, r := range cfg.Push {
		title := fmt.Sprintf("push %d → %d", r.Local, r.RemotePort())
		switch states[r.Local] {
		case "failed":
			rep.add(fail(title, errs[r.Local], "free the remote port or pick another (`mole push "+strconv.Itoa(r.Local)+":PORT`); retried after reconnect"))
			continue
		case "active":
		default:
			if live {
				rep.add(warn(title, "configured but not loaded by the running daemon", "`mole restart`, or re-add it with `mole push`"))
				continue
			}
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.Local)), 400*time.Millisecond)
		if err != nil {
			rep.add(warn(title, fmt.Sprintf("nothing listening on 127.0.0.1:%d", r.Local), "start your local service; remote clients would get a closed connection"))
			continue
		}
		_ = c.Close()
		state := "local service up"
		if states[r.Local] == "active" {
			state += ", remote listener open"
		}
		rep.add(ok(title, state))
	}
}

func runOut(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func doctorDomains(rep *reporter, cfg *config.Config, live bool) {
	if runtime.GOOS != "linux" {
		rep.add(info("split DNS", "only supported on Linux"))
		return
	}
	if len(cfg.Domains) == 0 {
		rep.add(warn("domains", "none configured", "add `domains: [example.lan]` to the config"))
		return
	}
	if _, err := runOut(2*time.Second, "systemctl", "is-active", "--quiet", "systemd-resolved"); err != nil {
		rep.add(fail("systemd-resolved", "not active", "mole dns needs systemd-resolved"))
		return
	}
	rep.add(ok("systemd-resolved", "active"))

	addr, err := runOut(2*time.Second, "ip", "-o", "-4", "addr", "show", "dev", "mole0")
	if err != nil || !strings.Contains(addr, meshdns.StubIP+"/32") {
		rep.add(fail("mole0 link", "missing", "run `sudo mole dns install`"))
	} else {
		rep.add(ok("mole0 link", meshdns.StubIP+" (dummy)"))
		status, _ := runOut(3*time.Second, "resolvectl", "status", "mole0")
		var missing []string
		for _, d := range cfg.Domains {
			if !strings.Contains(status, "~"+d) {
				missing = append(missing, d)
			}
		}
		switch {
		case !strings.Contains(status, meshdns.StubAddr):
			rep.add(fail("DNS routing", "mole0 does not point at "+meshdns.StubAddr, "run `sudo mole dns install`"))
		case len(missing) > 0:
			rep.add(fail("DNS routing", "not routed: "+strings.Join(missing, ", "), "domains: changed? run `sudo mole dns install` again"))
		default:
			rep.add(ok("DNS routing", "~"+strings.Join(cfg.Domains, " ~")+" → "+meshdns.StubAddr))
		}
		if !strings.Contains(status, "Default Route: no") {
			rep.add(warn("default route", "mole0 may capture all DNS", "run `sudo mole dns install` again"))
		}
	}

	if out, err := runOut(3*time.Second, "nft", "list", "table", "ip", "mole"); err != nil {
		if strings.Contains(out, "Operation not permitted") || strings.Contains(out, "Permission denied") {
			rep.add(info("nft redirect", "needs root to inspect"))
		} else {
			rep.add(fail("nft redirect", "table `ip mole` not loaded", "run `sudo mole dns install`"))
		}
	} else {
		var missing []string
		for _, p := range cfg.DomainPortsOrDefault() {
			if !strings.Contains(out, fmt.Sprintf("dport %d dnat to :%d", p, meshdns.BindPort(p))) {
				missing = append(missing, strconv.Itoa(p))
			}
		}
		if len(missing) > 0 {
			rep.add(fail("nft redirect", "ports not redirected: "+strings.Join(missing, ", "), "domain_ports: changed? run `sudo mole dns install` again"))
		} else {
			rep.add(ok("nft redirect", fmt.Sprintf("ports %v → +%d", cfg.DomainPortsOrDefault(), meshdns.PortShift)))
		}
	}

	switch stubAlive(meshdns.StubAddr) {
	case true:
		rep.add(ok("DNS stub", "answering on "+meshdns.StubAddr))
	default:
		if live {
			rep.add(fail("DNS stub", "mole up runs but the stub doesn't answer", "check `mole logs` for `dns stub bind failed`"))
		} else {
			rep.add(info("DNS stub", "not answering (mole up isn't running)"))
		}
	}
	if !live {
		for _, p := range cfg.DomainPortsOrDefault() {
			bp := meshdns.BindPort(p)
			ln, err := net.Listen("tcp", net.JoinHostPort("127.77.255.254", strconv.Itoa(bp)))
			if err != nil {
				rep.add(fail(fmt.Sprintf("port %d", p), fmt.Sprintf("mole's port %d is taken by another process", bp), "free it: `ss -ltnp | grep :"+strconv.Itoa(bp)+"`"))
				continue
			}
			_ = ln.Close()
		}
	}
	doctorDoH(rep)
}

// stubAlive sends a query for a name outside any domain (REFUSED by the
// stub, no side effects) and reports whether the stub replied.
func stubAlive(addr string) bool {
	c, err := net.DialTimeout("udp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	q := []byte{0xD0, 0x0C, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 6, 'd', 'o', 'c', 't', 'o', 'r', 7, 'i', 'n', 'v', 'a', 'l', 'i', 'd', 0, 0, 1, 0, 1}
	_ = c.SetDeadline(time.Now().Add(800 * time.Millisecond))
	if _, err := c.Write(q); err != nil {
		return false
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	return err == nil && n >= 12 && buf[0] == 0xD0 && buf[1] == 0x0C && buf[2]&0x80 != 0
}

// doctorDoH warns when a browser resolves names itself (DNS-over-HTTPS),
// bypassing the system resolver and so the split DNS.
func doctorDoH(rep *reporter) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	found := dohBrowsers(home)
	if len(found) == 0 {
		rep.add(ok("browser DoH", "no browser with DNS-over-HTTPS enabled found"))
		return
	}
	rep.add(warn("browser DoH", strings.Join(found, ", ")+" use DNS-over-HTTPS", "turn off Secure DNS / DoH in the browser, or the domain won't resolve there"))
}

func dohBrowsers(home string) []string {
	var found []string
	prefs, _ := filepath.Glob(filepath.Join(home, ".mozilla", "firefox", "*", "prefs.js"))
	users, _ := filepath.Glob(filepath.Join(home, ".mozilla", "firefox", "*", "user.js"))
	for _, f := range append(prefs, users...) {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, `"network.trr.mode"`) &&
				(strings.Contains(line, ", 2)") || strings.Contains(line, ", 3)")) {
				found = append(found, "Firefox")
				goto chromium
			}
		}
	}
chromium:
	for name, dir := range map[string]string{
		"Chrome":   "google-chrome",
		"Chromium": "chromium",
		"Brave":    filepath.Join("BraveSoftware", "Brave-Browser"),
	} {
		data, err := os.ReadFile(filepath.Join(home, ".config", dir, "Local State"))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), `"dns_over_https":{"mode":"secure"`) {
			found = append(found, name)
		}
	}
	return found
}

// doctorName probes one name end to end: the remote resolves it, the local
// resolver hands out a virtual IP, and the domain ports connect.
func doctorName(rep *reporter, cfg *config.Config, p sshProbe, name string) {
	title := name
	if p != nil {
		out, err := p.Run("getent ahosts " + shellQuote(name))
		if errors.Is(err, errProbeTimeout) {
			rep.add(fail(title, err.Error(), "the SSH session stopped answering; check the remote and `ssh -v`"))
			return
		}
		if err != nil || strings.TrimSpace(string(out)) == "" {
			rep.add(fail(title, "the remote can't resolve it", "check the name, or the remote's DNS (`getent hosts "+name+"` there)"))
			return
		}
		rep.add(ok(title, "remote resolves → "+firstField(string(out))))
	}
	out, err := runOut(4*time.Second, "resolvectl", "-4", "--synthesize=no", "--cache=no", "query", name)
	ip := ""
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "127.77.") {
			ip = f
			break
		}
	}
	if err != nil || ip == "" {
		rep.add(fail(title, "local DNS gave no virtual IP", "is the name under `domains:`? is mole up running? `sudo mole dns install`"))
		return
	}
	rep.add(ok(title, "local DNS → "+ip+" (virtual)"))
	for _, port := range cfg.DomainPortsOrDefault() {
		doctorPort(rep, p, name, ip, port)
	}
}

// doctorPort checks one name:port without overclaiming. mole's local
// listener accepts before it dials the destination, so a completed TCP
// handshake proves nothing about the tunnel: when that dial fails mole closes
// the connection, which shows up as an immediate EOF. The remote side is then
// verified separately by opening the same name:port over SSH.
func doctorPort(rep *reporter, p sshProbe, name, ip string, port int) {
	title := net.JoinHostPort(name, strconv.Itoa(port))
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		rep.add(fail(title, "connection failed: "+shortErr(err), "nft redirect missing or mole up not running: `sudo mole dns install` then `mole up`"))
		return
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	var one [1]byte
	if _, err := c.Read(one[:]); err != nil {
		var ne net.Error
		if !(errors.As(err, &ne) && ne.Timeout()) {
			rep.add(fail(title, "mole closed the connection: the remote can't reach the target", "is the service up on the remote? see `mole logs` for the dial error"))
			return
		}
	}
	if p == nil {
		rep.add(info(title, "local listener reachable (remote not verified: SSH is down)"))
		return
	}
	rc, err := p.Dial("tcp", title)
	if err != nil {
		rep.add(fail(title, "the remote can't open it: "+shortErr(err), "is the service up on the remote and listening on that port?"))
		return
	}
	_ = rc.Close()
	rep.add(ok(title, "local listener reachable, remote accepts the connection"))
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func shortErr(err error) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	return err.Error()
}
