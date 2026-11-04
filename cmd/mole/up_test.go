package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/tunnel"
	"golang.org/x/crypto/ssh"
)

// isolateUp gives a `mole up` run state, config and home directories of its
// own, no ssh-agent, and a working directory without a mole.yaml. It returns
// the home directory.
func isolateUp(t *testing.T) string {
	t.Helper()
	isolateState(t)
	home := os.Getenv("HOME")
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "config"))
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv(serviceEnv, "")
	t.Chdir(t.TempDir())
	return home
}

// sshRemote starts an in-process sshd that accepts a key it installs as
// ~/.ssh/id_ed25519 (call isolateUp first), so the default auth path of
// tunnel.New reaches it. It returns the server and its user@host:port.
func sshRemote(t *testing.T) (*sshTestServer, string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(os.Getenv("HOME"), ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newSSHTestServer(t, signer.PublicKey())
	return srv, "u@" + srv.ln.Addr().String()
}

// writeUpConfig writes a config file and returns its path.
func writeUpConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mole.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// upRun is a `mole up` server running on its own goroutine.
type upRun struct {
	t    *testing.T
	s    *upServer
	out  *syncBuf
	stop context.CancelFunc
	done chan struct{}
	code int
}

// newTestUp builds the server `mole up args...` would run, logging into a
// buffer, retrying the first connection quickly, dialling with dial (nil:
// the real tunnel.New).
func newTestUp(t *testing.T, dial func(tunnel.Remote, bool, *slog.Logger) (*tunnel.Manager, error), args ...string) (*upServer, *syncBuf) {
	t.Helper()
	flags, err := parseUpFlags(args, io.Discard)
	if err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	out := &syncBuf{}
	s := newUpServer(flags)
	s.stderr = out
	s.waitForInstance = false
	s.backoff = backoff{initial: 10 * time.Millisecond, max: 50 * time.Millisecond}
	if dial != nil {
		s.dial = dial
	}
	return s, out
}

// startUp runs s until the test stops it (or the test ends).
func startUp(t *testing.T, s *upServer, out *syncBuf) *upRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &upRun{t: t, s: s, out: out, stop: cancel, done: make(chan struct{})}
	go func() {
		r.code = s.run(ctx)
		close(r.done)
	}()
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return r
}

// wait returns the run's exit status once it has returned.
func (r *upRun) wait() int {
	r.t.Helper()
	select {
	case <-r.done:
		return r.code
	case <-time.After(20 * time.Second):
		r.t.Fatalf("mole up did not return; log:\n%s", r.out.String())
		return -1
	}
}

// upStatus is GET /status, decoded.
type upStatus struct {
	Info struct {
		State    string            `json:"state"`
		Remote   string            `json:"remote"`
		Ports    *[]int            `json:"ports"`
		Push     *[]admin.PushInfo `json:"push"`
		Routes   []admin.RouteInfo `json:"routes"`
		Transfer map[string]any    `json:"transfer"`
	} `json:"info"`
}

func getStatus(addr string) (upStatus, error) {
	var st upStatus
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/status")
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("status %s", resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// awaitState waits until /status at addr reports state.
func (r *upRun) awaitState(addr, state string) upStatus {
	r.t.Helper()
	var st upStatus
	eventually(r.t, "/status state "+state, func() bool {
		var err error
		st, err = getStatus(addr)
		return err == nil && st.Info.State == state
	})
	return st
}

func refusesConnections(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return true
	}
	c.Close()
	return false
}

// dialPong dials a forwarded local port and returns what came back.
func dialPong(port int) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	return string(b)
}

// assertReleased checks what every finished run must leave behind: no
// instance lock, no pidfile, nothing listening on addrs.
func assertReleased(t *testing.T, addrs ...string) {
	t.Helper()
	if held, err := lockHeld(); err != nil || held {
		t.Errorf("instance lock still held after the run: %v, %v", held, err)
	}
	if pid, ok := readPid(); ok {
		t.Errorf("pidfile left behind (pid %d)", pid)
	}
	for _, a := range addrs {
		if !refusesConnections(a) {
			t.Errorf("%s still accepts connections after the run", a)
		}
	}
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// The flags override the config file, which overrides the defaults; without
// -config, ./mole.yaml wins over the user-global config.
func TestUpConfigPrecedence(t *testing.T) {
	isolateUp(t)
	global := config.GlobalPath()
	if err := os.MkdirAll(filepath.Dir(global), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("remote: g@global\nports: [1111]\nadmin_addr: 127.0.0.1:1111\nlog_level: warn\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	load := func(args ...string) (*config.Config, string) {
		t.Helper()
		f, err := parseUpFlags(args, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		cfg, path, err := f.config()
		if err != nil {
			t.Fatalf("config(%v): %v", args, err)
		}
		return cfg, path
	}

	cfg, path := load()
	if path != global || cfg.Remote != "g@global" || !equalInts(cfg.Ports, []int{1111}) || cfg.AdminAddr != "127.0.0.1:1111" || cfg.LogLevel != "warn" {
		t.Fatalf("user-global config: path=%q %+v", path, cfg)
	}
	if cfg.AutoDiscover || cfg.Insecure || !equalInts(cfg.ExcludePorts, config.Default().ExcludePorts) {
		t.Fatalf("defaults not kept under the file: %+v", cfg)
	}

	if err := os.WriteFile(config.LocalPath, []byte("remote: l@local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, path = load(); path != config.LocalPath || cfg.Remote != "l@local" {
		t.Fatalf("./mole.yaml must win over the user-global config: path=%q remote=%q", path, cfg.Remote)
	}

	cfg, path = load("-config", global, "-remote", "f@flag", "-ports", "2,3", "-admin", "localhost:2222",
		"-log-level", "debug", "-auto-discover", "-insecure")
	if path != global || cfg.Remote != "f@flag" || !equalInts(cfg.Ports, []int{2, 3}) || cfg.AdminAddr != "localhost:2222" ||
		cfg.LogLevel != "debug" || !cfg.AutoDiscover || !cfg.Insecure {
		t.Fatalf("flags must override the file: path=%q %+v", path, cfg)
	}

	if f, _ := parseUpFlags([]string{"-d"}, io.Discard); !f.detached() {
		t.Error("-d is not detached")
	}
	if f, _ := parseUpFlags([]string{"-detach"}, io.Discard); !f.detached() {
		t.Error("-detach is not detached")
	}
}

// Configuration problems end the run with status 1 before anything listens
// or dials, and leave no claim behind; bad flags are status 2, -h is 0.
func TestUpConfigErrors(t *testing.T) {
	bad := writeUpConfig(t, "remote: [\n")
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"unreadable config", []string{"-config", bad}, "config error: parse config"},
		{"no remote", nil, "error: -remote is required (or set 'remote:' in config)"},
		{"admin on every interface", []string{"-remote", "u@h", "-admin", "0.0.0.0:9999"}, "config error: admin_addr \"0.0.0.0:9999\" listens on every network interface"},
		{"admin on a LAN address", []string{"-remote", "u@h", "-admin", "192.168.1.5:9999"}, "is not a loopback address"},
		{"invalid remote", []string{"-remote", "-oProxyCommand=x"}, `msg="invalid remote"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolateUp(t)
			s, out := newTestUp(t, func(tunnel.Remote, bool, *slog.Logger) (*tunnel.Manager, error) {
				t.Error("dialled despite a configuration error")
				return nil, errors.New("no")
			}, c.args...)
			if code := s.run(context.Background()); code != 1 {
				t.Fatalf("exit %d, want 1; output:\n%s", code, out)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Fatalf("output %q does not say %q", out, c.want)
			}
			assertReleased(t)
		})
	}

	t.Run("flags", func(t *testing.T) {
		isolateUp(t)
		var code int
		out := captureBoth(t, func() { code = runUp([]string{"-bogus"}) })
		if code != 2 || !strings.Contains(out, "flag provided but not defined: -bogus") || !strings.Contains(out, "Usage: mole up [flags]") {
			t.Fatalf("bad flag: exit %d, output:\n%s", code, out)
		}
		out = captureBoth(t, func() { code = runUp([]string{"-h"}) })
		if code != 0 || !strings.Contains(out, "Either -remote or a config file with 'remote:' is required.") {
			t.Fatalf("-h: exit %d, output:\n%s", code, out)
		}
		out = captureBoth(t, func() { code = runUp([]string{"-config", bad}) })
		if code != 1 || !strings.Contains(out, "config error:") {
			t.Fatalf("runUp with a bad config: exit %d, output:\n%s", code, out)
		}
	})
}

// A second server refuses to start (status 1) and leaves the first one's
// claim alone.
func TestUpRefusesASecondInstance(t *testing.T) {
	isolateUp(t)
	first, err := claimInstance(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()
	s, out := newTestUp(t, nil, "-remote", "u@127.0.0.1:1")
	if code := s.run(context.Background()); code != 1 || !strings.Contains(out.String(), "mole is already running") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if held, _ := lockHeld(); !held {
		t.Fatal("the first server lost its lock")
	}
	if pid, ok := readPid(); !ok || pid != os.Getpid() {
		t.Fatal("the first server lost its pidfile")
	}
}

// While the first connection is retried the admin API already answers:
// /status says "connecting" (with the shape clients parse), the endpoints
// that need the tunnel answer 503, the browser guard holds. Once connected
// the full API takes over; a stop then releases everything.
func TestUpAdminAnswersWhileConnecting(t *testing.T) {
	isolateUp(t)
	srv, remote := sshRemote(t)
	srv.backend = loopback(localEcho(t))
	adminAddr, pinned := loopback(freeLoopbackPort(t)), freeLoopbackPort(t)
	pushLocal, pushRemote := freeLoopbackPort(t), freeLoopbackPort(t)
	cfgPath := writeUpConfig(t, fmt.Sprintf(`remote: %s
insecure: true
admin_addr: %s
ports: [%d]
push:
  - local: %d
    remote: %d
domains: [mole.localhost]
route_port: %d
routes:
  web: "3000"
`, remote, adminAddr, pinned, pushLocal, pushRemote, freeLoopbackPort(t)))

	var attempts atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	dial := func(rem tunnel.Remote, insecure bool, log *slog.Logger) (*tunnel.Manager, error) {
		if attempts.Add(1) == 1 { // the network is not up yet
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		<-release
		return tunnel.New(rem, insecure, log)
	}
	s, out := newTestUp(t, dial, "-config", cfgPath)
	r := startUp(t, s, out)

	st := r.awaitState(adminAddr, stateConnecting)
	eventually(t, "a retry of the first connection", func() bool { return attempts.Load() >= 2 })
	if st.Info.Remote != remote || st.Info.Ports == nil || len(*st.Info.Ports) != 0 {
		t.Errorf("connecting status: remote %q ports %v", st.Info.Remote, st.Info.Ports)
	}
	if st.Info.Push == nil || len(*st.Info.Push) != 1 || (*st.Info.Push)[0].Local != pushLocal || (*st.Info.Push)[0].Remote != pushRemote ||
		(*st.Info.Push)[0].State != stateConnecting {
		t.Errorf("connecting status: push %+v", st.Info.Push)
	}
	if len(st.Info.Routes) != 1 || st.Info.Routes[0].Name != "web" {
		t.Errorf("connecting status: routes %+v", st.Info.Routes)
	}
	if on, ok := st.Info.Transfer["receive"].(bool); !ok || on {
		t.Errorf("connecting status: transfer %+v", st.Info.Transfer)
	}
	if !daemonLive(adminAddr) {
		t.Error("/health does not answer while connecting")
	}
	live := freeLoopbackPort(t)
	err := adminPostPort(adminAddr, live)
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "still connecting to "+remote) {
		t.Errorf("POST /ports/discover while connecting: %v, want a 503 that says why", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+adminAddr+"/status", nil)
	req.Header.Set("Origin", "https://attacker.example")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a browser request while connecting: %v %v, want 403", resp, err)
	} else {
		resp.Body.Close()
	}
	if !refusesConnections(loopback(pinned)) {
		t.Error("a port is forwarded before the connection is up")
	}

	releaseOnce.Do(func() { close(release) })
	st = r.awaitState(adminAddr, stateConnected)
	eventually(t, "the pinned port in /status", func() bool {
		st, _ = getStatus(adminAddr)
		return st.Info.Ports != nil && contains(*st.Info.Ports, pinned)
	})
	if got := dialPong(pinned); got != "pong" {
		t.Errorf("pinned port through the tunnel answered %q", got)
	}
	// The full API took over: a live add works now.
	if err := adminPostPort(adminAddr, live); err != nil {
		t.Errorf("POST /ports/discover once connected: %v", err)
	}
	if got := dialPong(live); got != "pong" {
		t.Errorf("live-added port answered %q", got)
	}

	r.stop()
	if code := r.wait(); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, out)
	}
	assertReleased(t, adminAddr, loopback(pinned), loopback(live))
	log := out.String()
	for _, want := range []string{connectRetryMessage, "admin listening", "mole up. Press Ctrl+C to stop.", "shutting down"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// A stop while the first connection is still being retried ends the run at
// once with status 0 and releases the admin port and the claim.
func TestUpStopWhileConnecting(t *testing.T) {
	isolateUp(t)
	adminAddr := loopback(freeLoopbackPort(t))
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	dial := func(tunnel.Remote, bool, *slog.Logger) (*tunnel.Manager, error) {
		<-blocked // a dial that hangs (no network); it ends with the test
		return nil, errors.New("gave up")
	}
	s, out := newTestUp(t, dial, "-remote", "u@127.0.0.1:1", "-admin", adminAddr)
	r := startUp(t, s, out)
	r.awaitState(adminAddr, stateConnecting)

	r.stop()
	if code := r.wait(); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, out)
	}
	assertReleased(t, adminAddr)
	if !strings.Contains(out.String(), "shutting down") {
		t.Errorf("no shutdown logged:\n%s", out)
	}
}

// A failure no retry can fix (authentication) ends the run with status 1,
// and the admin API that answered meanwhile goes away with it.
func TestUpPermanentConnectFailure(t *testing.T) {
	isolateUp(t)
	adminAddr := loopback(freeLoopbackPort(t))
	dial := func(tunnel.Remote, bool, *slog.Logger) (*tunnel.Manager, error) {
		return nil, errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]")
	}
	s, out := newTestUp(t, dial, "-remote", "u@127.0.0.1:1", "-admin", adminAddr)
	if code := s.run(context.Background()); code != 1 {
		t.Fatalf("exit %d; log:\n%s", code, out)
	}
	if !strings.Contains(out.String(), `msg="ssh connect failed"`) {
		t.Errorf("log:\n%s", out)
	}
	assertReleased(t, adminAddr)
}

// End to end over a real SSH connection to an in-process sshd: `mole up`
// with a pinned port, a discovered port, a push forward and a named route
// comes up, carries traffic both ways, and on stop releases every listener,
// the remote's push listener and the instance claim.
func TestUpEndToEnd(t *testing.T) {
	isolateUp(t)
	srv, remote := sshRemote(t)
	echo := localEcho(t)
	srv.backend = loopback(echo)
	adminPort, pinned, discovered, pushRemote, routePort := freeLoopbackPort(t), freeLoopbackPort(t), freeLoopbackPort(t), freeLoopbackPort(t), freeLoopbackPort(t)
	// The remote lists one listener: discovered.
	srv.exec = func(cmd string, ch ssh.Channel) uint32 {
		if cmd == "ss -tlnH" {
			fmt.Fprintf(ch, "LISTEN 0 4096 127.0.0.1:%d 0.0.0.0:*\n", discovered)
			return 0
		}
		fmt.Fprintf(ch.Stderr(), "%s: command not found\n", cmd)
		return 127
	}
	cfgPath := writeUpConfig(t, fmt.Sprintf(`remote: %s
insecure: true
log_level: debug
admin_addr: 127.0.0.1:%d
ports: [%d]
auto_discover: true
discover_ports: []
push:
  - local: %d
    remote: %d
domains: [mole.localhost]
route_port: %d
routes:
  web: "3000"
`, remote, adminPort, pinned, echo, pushRemote, routePort))

	s, out := newTestUp(t, nil, "-config", cfgPath)
	r := startUp(t, s, out)
	adminAddr := loopback(adminPort)
	st := r.awaitState(adminAddr, stateConnected)
	eventually(t, "the pinned and the discovered port in /status", func() bool {
		st, _ = getStatus(adminAddr)
		return st.Info.Ports != nil && contains(*st.Info.Ports, pinned) && contains(*st.Info.Ports, discovered)
	})
	if st.Info.Push == nil || len(*st.Info.Push) != 1 || (*st.Info.Push)[0].State != "active" {
		t.Errorf("push in /status: %+v", st.Info.Push)
	}
	if len(st.Info.Routes) != 1 || st.Info.Routes[0].Target != "127.0.0.1:3000" {
		t.Errorf("routes in /status: %+v", st.Info.Routes)
	}

	for _, p := range []int{pinned, discovered} {
		if got := dialPong(p); got != "pong" {
			t.Errorf("forwarded port %d answered %q", p, got)
		}
	}
	if got := remotePing(pushRemote); got != "pong" {
		t.Errorf("push forward on the remote answered %q", got)
	}
	if refusesConnections(loopback(routePort)) {
		t.Error("the route proxy does not listen")
	}

	r.stop()
	if code := r.wait(); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, out)
	}
	assertReleased(t, adminAddr, loopback(pinned), loopback(discovered), loopback(routePort))
	eventually(t, "the remote's push listener closed", func() bool { return remotePing(pushRemote) == "" })

	log := out.String()
	for _, want := range []string{
		fmt.Sprintf("msg=forwarding local=127.0.0.1:%d", pinned),
		fmt.Sprintf("msg=forwarding local=127.0.0.1:%d", discovered),
		"msg=pushing", "route proxy listening", "mole up. Press Ctrl+C to stop.", "shutting down",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}
