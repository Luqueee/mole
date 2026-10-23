package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/meshdns"
	"github.com/Luqueee/mole/internal/proxy"
	"github.com/Luqueee/mole/internal/tunnel"
	"golang.org/x/crypto/ssh"
)

func TestReporterCountsAndExitCode(t *testing.T) {
	var buf bytes.Buffer
	r := newReporter(&buf, false)
	r.section("SSH")
	r.add(ok("connect", "fast"))
	r.add(warn("push", "meh", "do the thing"))
	r.add(info("daemon", "off"))
	if code := r.summary(); code != 0 {
		t.Fatalf("exit = %d with no failures", code)
	}
	r.add(fail("boom", "bad", "fix it"))
	if code := r.summary(); code != 1 {
		t.Fatalf("exit = %d with a failure", code)
	}
	out := buf.String()
	for _, want := range []string{"✓  connect", "⚠  push", "↳ do the thing", "○  daemon", "✗  boom", "1 failed", "1 warning"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("colour codes leaked into a non-colour report")
	}
}

func TestClassifySSHError(t *testing.T) {
	for msg, want := range map[string]string{
		"no SSH auth methods: set up ssh-agent": "ssh-add",
		"ssh: unable to authenticate":           "authorized_keys",
		"host key mismatch for x":               "known_hosts",
		"dial tcp: connection refused":          "sshd",
		"dial tcp: i/o timeout":                 "VPN",
	} {
		if _, hint := classifySSHError(errors.New(msg), "h"); !strings.Contains(hint, want) {
			t.Errorf("%q: hint %q lacks %q", msg, hint, want)
		}
	}
}

type fakeProbe struct {
	dialErr   error
	listenErr error
}

func (f fakeProbe) Dial(string, string) (net.Conn, error) {
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	c, _ := net.Pipe()
	return c, nil
}
func (f fakeProbe) Listen(string, string) (net.Listener, error) {
	if f.listenErr != nil {
		return nil, f.listenErr
	}
	return net.Listen("tcp", "127.0.0.1:0")
}
func (fakeProbe) Run(string) ([]byte, error) { return nil, nil }

func levels(f func(*reporter)) [4]int {
	r := newReporter(io.Discard, false)
	f(r)
	return r.counts
}

func TestDoctorForwarding(t *testing.T) {
	rem := tunnel.Remote{Addr: "h:22"}
	prohibited := &ssh.OpenChannelError{Reason: ssh.Prohibited}
	refused := &ssh.OpenChannelError{Reason: ssh.ConnectionFailed}
	denied := errors.New("ssh: tcpip-forward request denied by peer")

	if c := levels(func(r *reporter) { doctorForwarding(r, fakeProbe{}, rem, true) }); c[lvFail] != 0 || c[lvOK] != 2 {
		t.Errorf("healthy: %v", c)
	}
	if c := levels(func(r *reporter) { doctorForwarding(r, fakeProbe{dialErr: prohibited}, rem, false) }); c[lvFail] != 1 {
		t.Errorf("AllowTcpForwarding off must fail pull: %v", c)
	}
	// the channel opened but the target port refused: forwarding is allowed
	if c := levels(func(r *reporter) { doctorForwarding(r, fakeProbe{dialErr: refused}, rem, false) }); c[lvFail] != 0 {
		t.Errorf("refused target is not a forwarding failure: %v", c)
	}
	// remote listener denied: fail only when push is configured
	if c := levels(func(r *reporter) { doctorForwarding(r, fakeProbe{listenErr: denied}, rem, true) }); c[lvFail] != 1 {
		t.Errorf("push configured + denied: %v", c)
	}
	if c := levels(func(r *reporter) { doctorForwarding(r, fakeProbe{listenErr: denied}, rem, false) }); c[lvFail] != 0 || c[lvWarn] != 1 {
		t.Errorf("push not configured + denied must only warn: %v", c)
	}
}

func TestStubAlive(t *testing.T) {
	if stubAlive("127.0.0.1:1") {
		t.Fatal("reported alive with nothing listening")
	}
	m := meshdns.New([]string{"kena.lan"}, nil, &net.Dialer{}, proxy.Hooks{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go m.ServeUDP(pc)
	if !stubAlive(pc.LocalAddr().String()) {
		t.Fatal("stub not detected")
	}
	if len(m.List()) != 0 {
		t.Fatalf("the probe must not allocate names: %v", m.List())
	}
}

func TestDohBrowsers(t *testing.T) {
	home := t.TempDir()
	if got := dohBrowsers(home); len(got) != 0 {
		t.Fatalf("clean home: %v", got)
	}
	ff := filepath.Join(home, ".mozilla", "firefox", "abc.default")
	cr := filepath.Join(home, ".config", "google-chrome")
	for dir, files := range map[string]map[string]string{
		ff: {"prefs.js": "user_pref(\"network.trr.mode\", 2);\n"},
		cr: {"Local State": `{"dns_over_https":{"mode":"secure","templates":""}}`},
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := strings.Join(dohBrowsers(home), ",")
	if !strings.Contains(got, "Firefox") || !strings.Contains(got, "Chrome") {
		t.Fatalf("got %q", got)
	}
	// trr.mode 0 / 5 means off
	if err := os.WriteFile(filepath.Join(ff, "prefs.js"), []byte("user_pref(\"network.trr.mode\", 5);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(dohBrowsers(home), ","), "Firefox") {
		t.Fatal("trr.mode 5 (off) flagged")
	}
}

func TestDoctorPushReportsDaemonFailures(t *testing.T) {
	f := newTestPush(&fakeRemote{failErr: errors.New("denied")})
	f.start([]config.PushRule{{Local: 3000}})
	srv := httptest.NewServer(admin.New(admin.NewStats(), map[string]any{}).WithPushController(f).Handler())
	defer srv.Close()
	cfg := &config.Config{AdminAddr: strings.TrimPrefix(srv.URL, "http://"), Push: []config.PushRule{{Local: 3000}}}

	c := levels(func(r *reporter) { doctorPush(r, cfg, true) })
	if c[lvFail] != 1 {
		t.Fatalf("failed remote bind must be a failure: %v", c)
	}
}

type dialerFunc func(network, addr string) (net.Conn, error)

func (f dialerFunc) Dial(n, a string) (net.Conn, error) { return f(n, a) }

// probeWith adapts a dial function to sshProbe (the SSH side of doctorPort).
type probeWith struct {
	fakeProbe
	dial func(string, string) (net.Conn, error)
}

func (p probeWith) Dial(n, a string) (net.Conn, error) { return p.dial(n, a) }

// A completed TCP handshake with mole's listener must not be reported as a
// working tunnel: the listener accepts before dialling the destination.
func TestDoctorPortDoesNotOverclaim(t *testing.T) {
	refused := &ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "connection refused"}
	hold := func(string, string) (net.Conn, error) { c, _ := net.Pipe(); return c, nil }
	fail503 := func(string, string) (net.Conn, error) { return nil, refused }

	bindPort := freeLoopbackPort(t)
	port := bindPort - meshdns.PortShift

	run := func(meshDial func(string, string) (net.Conn, error), probe sshProbe) [4]int {
		m := meshdns.New([]string{"kena.lan"}, []int{port}, dialerFunc(meshDial), proxy.Hooks{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer m.Close()
		ip, err := m.Resolve("admin.kena.lan")
		if err != nil {
			t.Fatal(err)
		}
		// no nft in the test: dial the shifted port directly, as DNAT would
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "0"))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() { // stand-in for the DNAT: forward to mole's real listener
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				up, err := net.Dial("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(bindPort)))
				if err != nil {
					c.Close()
					continue
				}
				go func() { _, _ = io.Copy(up, c); up.Close() }()
				go func() { _, _ = io.Copy(c, up); c.Close() }()
			}
		}()
		return levels(func(r *reporter) {
			doctorPort(r, probe, "admin.kena.lan", "127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
		})
	}

	// target unreachable from the remote: mole closes the connection → fail,
	// even though the TCP handshake with the local listener succeeded
	if c := run(fail503, probeWith{dial: fail503}); c[lvFail] != 1 || c[lvOK] != 0 {
		t.Errorf("refused destination: %v", c)
	}
	// everything reachable → ok
	if c := run(hold, probeWith{dial: hold}); c[lvOK] != 1 || c[lvFail] != 0 {
		t.Errorf("healthy: %v", c)
	}
	// tunnel works locally but SSH can't verify the remote → never a green ok
	if c := run(hold, nil); c[lvOK] != 0 || c[lvInfo] != 1 {
		t.Errorf("unverified remote must not be ok: %v", c)
	}
	// mole's dial succeeds but the doctor's own SSH probe is refused → fail
	if c := run(hold, probeWith{dial: fail503}); c[lvFail] != 1 {
		t.Errorf("remote probe refused: %v", c)
	}
}

// An SSH server that authenticates and then stops answering must not hang
// the diagnosis: every post-login probe is bounded by -timeout.
func TestDoctorBoundsSSHProbesOnStalledServer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	keyFile := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newSSHTestServer(t, signer.PublicKey())
	srv.stall = true

	rem := tunnel.Remote{User: "u", Addr: srv.ln.Addr().String(), IdentityFiles: []string{keyFile}}
	m, err := tunnel.New(rem, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	probe := boundedProbe{p: m, d: 300 * time.Millisecond, abort: func() { _ = m.Close() }, dead: new(atomic.Bool)}

	var counts [4]int
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		counts = levels(func(r *reporter) {
			doctorForwarding(r, probe, rem, true)
			doctorName(r, &config.Config{}, probe, "admin.kena.lan")
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("doctor hung on a stalled SSH session")
	}
	if counts[lvFail] != 3 {
		t.Fatalf("want pull+push+name failures, got %v", counts)
	}
	// only the first probe waits for the deadline; the rest fail fast
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("took %s, later probes should fail immediately", el)
	}
}

// tcpip-forward accepted but cancel-tcpip-forward never answered: closing the
// probe listener blocks in x/crypto, so doctor must bound that Close too.
func TestDoctorBoundsCancelIgnoredByServer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	keyFile := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newSSHTestServer(t, signer.PublicKey())
	srv.ignoreCancel = true

	rem := tunnel.Remote{User: "u", Addr: srv.ln.Addr().String(), IdentityFiles: []string{keyFile}}
	m, err := tunnel.New(rem, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	probe := boundedProbe{p: m, d: 300 * time.Millisecond, abort: func() { _ = m.Close() }, dead: new(atomic.Bool)}

	var counts [4]int
	done := make(chan struct{})
	go func() {
		defer close(done)
		counts = levels(func(r *reporter) { doctorForwarding(r, probe, rem, true) })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("doctor hung closing the remote probe listener")
	}
	if counts[lvWarn] != 1 || counts[lvFail] != 0 {
		t.Fatalf("want one warning (cancel unanswered), got %v", counts)
	}
}
