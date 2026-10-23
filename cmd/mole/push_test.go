package main

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/proxy"
)

// fakeRemote stands in for the SSH client: Listen opens a loopback listener
// (the "remote" side); kill closes them all like a dropped transport.
type fakeRemote struct {
	mu      sync.Mutex
	lns     []net.Listener
	addrs   []string
	failErr error
}

func (r *fakeRemote) Listen(_, addr string) (net.Listener, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failErr != nil {
		return nil, r.failErr
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r.lns = append(r.lns, ln)
	r.addrs = append(r.addrs, addr)
	return ln, nil
}

func (r *fakeRemote) kill() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ln := range r.lns {
		_ = ln.Close()
	}
}

// gatedRemote blocks its first Listen until released, so a reconnect can be
// simulated while start() is still binding.
type gatedRemote struct {
	fakeRemote
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedRemote) Listen(n, a string) (net.Listener, error) {
	g.once.Do(func() { close(g.entered); <-g.release })
	return g.fakeRemote.Listen(n, a)
}

// A reconnect that fires while start() is mid-bind must not be lost: the
// hook is registered first and reopenAll serialises behind start.
func TestPushReconnectDuringStartIsNotLost(t *testing.T) {
	remote := &gatedRemote{entered: make(chan struct{}), release: make(chan struct{})}
	f := newTestPush(remote)
	defer f.closeAll()

	done := make(chan struct{})
	go func() { f.start([]config.PushRule{{Local: 3000}, {Local: 3001}}); close(done) }()
	<-remote.entered

	remote.kill() // transport dropped mid-bind
	hook := make(chan struct{})
	go func() { f.reopenAll(); close(hook) }() // OnReconnect fired now
	close(remote.release)
	<-done
	<-hook

	for _, s := range f.ListPush() {
		if s.State != "active" {
			t.Fatalf("push %d left %s: %s", s.Local, s.State, s.Error)
		}
	}
}

func (r *fakeRemote) last() net.Listener {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lns[len(r.lns)-1]
}

// localEcho starts a local service that replies "pong" and returns its port.
func localEcho(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("pong"))
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func ping(t *testing.T, ln net.Listener) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	return string(b)
}

func newTestPush(r remoteListener) *pushForwarder {
	return newPushForwarder(r, slog.New(slog.NewTextHandler(io.Discard, nil)), proxy.Hooks{})
}

func TestPushReachesLocalAndSurvivesReconnect(t *testing.T) {
	local := localEcho(t)
	remote := &fakeRemote{}
	f := newTestPush(remote)
	defer f.closeAll()
	f.start([]config.PushRule{{Local: local, Remote: 8080}})

	if got := ping(t, remote.last()); got != "pong" {
		t.Fatalf("before kill: got %q", got)
	}
	if remote.addrs[0] != "127.0.0.1:8080" {
		t.Fatalf("remote bind = %q", remote.addrs[0])
	}

	remote.kill() // transport died
	f.reopenAll() // Manager.OnReconnect

	if got := ping(t, remote.last()); got != "pong" {
		t.Fatalf("after reconnect: got %q", got)
	}
	if st := f.ListPush(); len(st) != 1 || st[0].State != "active" {
		t.Fatalf("state = %+v", st)
	}
}

func TestPushRemoteBindFailureReportedAndRetried(t *testing.T) {
	remote := &fakeRemote{failErr: errors.New("tcpip-forward request denied")}
	f := newTestPush(remote)
	f.start([]config.PushRule{{Local: 3000}})

	st := f.ListPush()
	if len(st) != 1 || st[0].State != "failed" || !strings.Contains(st[0].Error, "denied") {
		t.Fatalf("state = %+v", st)
	}

	remote.mu.Lock()
	remote.failErr = nil
	remote.mu.Unlock()
	f.reopenAll()
	defer f.closeAll()
	if st := f.ListPush(); st[0].State != "active" {
		t.Fatalf("after retry: %+v", st)
	}
}

func TestPushAddRemove(t *testing.T) {
	remote := &fakeRemote{}
	f := newTestPush(remote)
	defer f.closeAll()

	if err := f.AddPush(3000, 8080, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.AddPush(3000, 9090, ""); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("duplicate err = %v", err)
	}
	ln := remote.last()
	if err := f.RemovePush(3000); err != nil {
		t.Fatal(err)
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("remote listener not closed: %v", err)
	}
	if len(f.ListPush()) != 0 {
		t.Fatal("rule still listed")
	}
	if err := f.RemovePush(3000); err != nil {
		t.Fatalf("remove not idempotent: %v", err)
	}

	remote.failErr = errors.New("nope")
	// A failed live add is reported but stays registered (state failed)...
	if err := f.AddPush(4000, 4000, ""); err == nil {
		t.Fatal("bind failure not reported")
	}
	if st := f.ListPush(); len(st) != 1 || st[0].State != "failed" {
		t.Fatalf("failed add must stay registered as failed: %+v", st)
	}
	// ...and is retried after the next reconnect.
	remote.mu.Lock()
	remote.failErr = nil
	remote.mu.Unlock()
	f.reopenAll()
	if st := f.ListPush(); st[0].State != "active" {
		t.Fatalf("not retried after reconnect: %+v", st)
	}
}

func TestPushAdminEndpoints(t *testing.T) {
	remote := &fakeRemote{}
	f := newTestPush(remote)
	defer f.closeAll()
	srv := httptest.NewServer(admin.New(admin.NewStats(), map[string]any{}).WithPushController(f).Handler())
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	if err := adminDo("POST", addr, "/ports/push", `{"local":3000,"remote":8080}`, 201); err != nil {
		t.Fatal(err)
	}
	if err := adminDo("POST", addr, "/ports/push", `{"local":3000}`, 409); err != nil {
		t.Fatal(err)
	}
	if err := adminDo("POST", addr, "/ports/push", `{"local":0}`, 400); err != nil {
		t.Fatal(err)
	}
	infos, err := fetchPushStatus(addr)
	if err != nil || len(infos) != 1 || infos[0].Remote != 8080 || infos[0].State != "active" {
		t.Fatalf("status = %+v, %v", infos, err)
	}
	if err := adminDo("DELETE", addr, "/ports/push/3000", "", 200); err != nil {
		t.Fatal(err)
	}
	if infos, _ := fetchPushStatus(addr); len(infos) != 0 {
		t.Fatalf("after delete: %+v", infos)
	}
}

func TestParsePushSpec(t *testing.T) {
	for in, want := range map[string][2]int{"3000": {3000, 3000}, "3000:8080": {3000, 8080}} {
		l, r, err := parsePushSpec(in)
		if err != nil || l != want[0] || r != want[1] {
			t.Errorf("%q -> %d,%d,%v", in, l, r, err)
		}
	}
	for _, bad := range []string{"", "x", "3000:", "0", "3000:70000", "3000:x"} {
		if _, _, err := parsePushSpec(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPushCLIPersistsConfig(t *testing.T) {
	path := t.TempDir() + "/mole.yaml"
	if rc := runPush([]string{"-config", path, "3000:8080"}); rc != 0 {
		t.Fatalf("push rc=%d", rc)
	}
	cfg, err := config.Load(path)
	if err != nil || len(cfg.Push) != 1 || cfg.Push[0].Local != 3000 || cfg.Push[0].RemotePort() != 8080 {
		t.Fatalf("cfg = %+v, %v", cfg.Push, err)
	}
	if rc := runPush([]string{"-config", path, "3000"}); rc != 0 { // replace, not duplicate
		t.Fatalf("push rc=%d", rc)
	}
	cfg, _ = config.Load(path)
	if len(cfg.Push) != 1 || cfg.Push[0].RemotePort() != 3000 {
		t.Fatalf("after replace = %+v", cfg.Push)
	}
	if rc := runUnpush([]string{"-config", path, strconv.Itoa(3000)}); rc != 0 {
		t.Fatalf("unpush rc=%d", rc)
	}
	cfg, _ = config.Load(path)
	if len(cfg.Push) != 0 {
		t.Fatalf("after unpush = %+v", cfg.Push)
	}
}
