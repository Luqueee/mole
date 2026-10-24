package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
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
	"github.com/Luqueee/mole/internal/proxy"
	"github.com/Luqueee/mole/internal/transfer"
	"github.com/Luqueee/mole/internal/tunnel"
	"golang.org/x/crypto/ssh"
)

// e2e is a local "machine" (daemon + hub) wired to a remote "machine" (an
// in-process sshd whose exec handler runs the same code `mole recv` runs)
// over a real SSH transport.
type e2e struct {
	t           *testing.T
	srv         *sshTestServer
	pf          *pushForwarder
	hub         *transferHub
	localInbox  string
	remoteInbox string
	localState  string
	remoteState string // the remote's state dir, i.e. <base>/mole
	remotePort  int
	oldRemote   atomic.Bool // the remote's mole predates `recv`
	logs        *syncBuf
	notes       syncBuf // what the hub would have shown as notifications
}

func newE2E(t *testing.T, tc config.TransferConfig) *e2e {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	e := &e2e{t: t, logs: &syncBuf{}}
	base := t.TempDir()
	e.localInbox = filepath.Join(base, "local-inbox")
	e.remoteInbox = filepath.Join(base, "remote-inbox")
	e.localState = filepath.Join(base, "local-state")
	e.remoteState = filepath.Join(base, "remote", "mole")

	remoteCfg := config.Default()
	remoteCfg.Transfer.Inbox = e.remoteInbox

	e.srv = newSSHTestServer(t, signer.PublicKey())
	e.srv.exec = func(cmd string, ch ssh.Channel) uint32 {
		if e.oldRemote.Load() && (cmd == remoteRecvCmd || cmd == remoteSetTokenCmd) {
			// What a mole predating `recv` does: complain on stderr, then
			// print its usage banner on stdout.
			fmt.Fprintln(ch.Stderr(), "unknown command: recv")
			fmt.Fprint(ch, "\n  mole  v0.1.1\n  local-port forwarder\n")
			return 1
		}
		switch cmd {
		case remoteRecvCmd:
			if err := recvStdio(context.Background(), remoteCfg, ch); err != nil {
				fmt.Fprintln(ch.Stderr(), "mole recv:", err)
				return 1
			}
			return 0
		case remoteSetTokenCmd:
			if err := recvSetToken(ch, e.remoteState); err != nil {
				fmt.Fprintln(ch.Stderr(), "mole recv:", err)
				return 1
			}
			return 0
		}
		fmt.Fprintf(ch.Stderr(), "bash: %s: command not found\n", cmd)
		return 127
	}

	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr, err := tunnel.New(tunnel.Remote{User: "u", Addr: e.srv.ln.Addr().String(), IdentityFiles: []string{keyFile}}, true, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go mgr.Watch(ctx, 50*time.Millisecond)

	e.pf = newPushForwarder(mgr, log, proxy.Hooks{})
	t.Cleanup(e.pf.closeAll)
	mgr.OnReconnect(e.pf.reopenAll)

	tc.Inbox = e.localInbox
	if tc.Port == 0 {
		tc.Port = freeLoopbackPort(t)
	}
	e.remotePort = freeLoopbackPort(t)
	e.hub = newTransferHub(log, "devlabs", "", tc, mgr, e.pf, e.localState)
	e.hub.remotePort = e.remotePort
	e.hub.notify = func(title, text string) { fmt.Fprintf(&e.notes, "%s|%s\n", title, text) }
	t.Cleanup(e.hub.close)
	mgr.OnReconnect(e.hub.onReconnect)
	return e
}

func (e *e2e) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	e.t.Cleanup(cancel)
	return ctx
}

func (e *e2e) remoteToken() string {
	tok, err := transfer.ReadToken(transfer.TokenPath(e.remoteState))
	if err != nil {
		return ""
	}
	return tok
}

// onRemote points StateDir() at the remote's state directory, so code that
// reads it (sendDirect) behaves as it would when run on the remote.
func (e *e2e) onRemote() {
	base := filepath.Dir(e.remoteState)
	e.t.Setenv("XDG_STATE_HOME", base)
	e.t.Setenv("LOCALAPPDATA", base)
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTransferToRemoteInbox(t *testing.T) {
	e := newE2E(t, config.TransferConfig{})
	data := bytes.Repeat([]byte("mole "), 100000)
	res, err := e.hub.Send(e.ctx(), transfer.FileInfo{Name: "notes.txt", Size: int64(len(data)), Mode: 0o644}, bytes.NewReader(data), "docs")
	if err != nil {
		t.Fatalf("%v\n%s", err, e.logs.String())
	}
	if res.Saved != "notes.txt" || res.Bytes != int64(len(data)) {
		t.Fatalf("result: %+v", res)
	}
	if got := readFile(t, filepath.Join(e.remoteInbox, "docs", "notes.txt")); got != string(data) {
		t.Fatal("content differs on the remote")
	}
	recs, _, _ := e.hub.Log(0)
	if len(recs) != 1 || recs[0].Dir != transfer.DirSend || recs[0].Status != transfer.StatusOK || recs[0].Peer != "devlabs" {
		t.Fatalf("log: %+v", recs)
	}
	// Same name again: the remote keeps both.
	res, err = e.hub.Send(e.ctx(), transfer.FileInfo{Name: "notes.txt", Size: 3}, strings.NewReader("abc"), "docs")
	if err != nil || res.Saved != "notes (1).txt" {
		t.Fatalf("second send: %+v %v", res, err)
	}
}

func TestTransferRefusedByRemote(t *testing.T) {
	e := newE2E(t, config.TransferConfig{})
	_, err := e.hub.Send(e.ctx(), transfer.FileInfo{Name: "x", Size: 1}, strings.NewReader("x"), "../escape")
	var re *transfer.RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("want the remote's refusal, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(filepath.Dir(e.remoteInbox), "escape")); serr == nil {
		t.Fatal("a path outside the remote inbox was created")
	}
}

func TestOldRemoteExplainsItself(t *testing.T) {
	e := newE2E(t, config.TransferConfig{Accept: "auto"})
	e.oldRemote.Store(true)
	_, err := e.hub.Send(e.ctx(), transfer.FileInfo{Name: "x", Size: 1}, strings.NewReader("x"), "")
	if err == nil || !strings.Contains(err.Error(), "unknown command: recv") || !strings.Contains(err.Error(), "mole update") {
		t.Fatalf("the remote's own message should reach the user: %v", err)
	}
	if strings.Contains(err.Error(), "local-port forwarder") {
		t.Fatalf("the usage banner leaked into the error: %v", err)
	}
	if err := e.hub.SetReceive(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "remote_error reported", func() bool {
		s, _ := e.hub.Info()["remote_error"].(string)
		return strings.Contains(s, "unknown command")
	})
	e.oldRemote.Store(false)
	if err := e.hub.SetReceive(true); err != nil { // refreshes the remote's copy
		t.Fatal(err)
	}
	eventually(t, "remote_error cleared once the remote works", func() bool {
		s, _ := e.hub.Info()["remote_error"].(string)
		return s == ""
	})
}

func TestUploadThroughDaemonAPI(t *testing.T) {
	e := newE2E(t, config.TransferConfig{})
	api := httptest.NewServer(admin.New(admin.NewStats(), nil).WithTransferController(e.hub).Handler())
	defer api.Close()
	addr := strings.TrimPrefix(api.URL, "http://")

	for _, c := range []struct {
		name string
		data string
	}{{"hello.txt", "hello over http"}, {"empty.txt", ""}} {
		f, err := transfer.FileFromPath(writeTemp(t, c.name, []byte(c.data)))
		if err != nil {
			t.Fatal(err)
		}
		res, err := uploadToDaemon(e.ctx(), addr, f, "")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.Saved != c.name || readFile(t, filepath.Join(e.remoteInbox, c.name)) != c.data {
			t.Fatalf("%s: result %+v", c.name, res)
		}
	}
	f, _ := transfer.FileFromPath(writeTemp(t, "bad", []byte("x")))
	if _, err := uploadToDaemon(e.ctx(), addr, f, "../up"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a refused upload should say so: %v", err)
	}
}

func TestReceiveFromRemoteDirect(t *testing.T) {
	e := newE2E(t, config.TransferConfig{Accept: "auto"})
	if err := e.hub.SetReceive(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "token installed on the remote", func() bool { return e.remoteToken() != "" })
	local, _ := transfer.ReadToken(transfer.TokenPath(e.localState))
	if local != e.remoteToken() {
		t.Fatal("the remote holds a different token")
	}
	if port, err := transfer.ReadPeerPort(transfer.PeerPortPath(e.remoteState)); err != nil || port != e.remotePort {
		t.Fatalf("remote peer port %d (%v), want %d", port, err, e.remotePort)
	}
	if len(e.pf.ListPush()) != 0 {
		t.Fatalf("mole's own forward shows up as a user push: %+v", e.pf.ListPush())
	}

	e.onRemote()
	f, err := transfer.FileFromPath(writeTemp(t, "from-remote.txt", []byte("sent from devlabs")))
	if err != nil {
		t.Fatal(err)
	}
	if code := sendDirect(e.ctx(), config.Default(), []transfer.File{f}, "inbound"); code != 0 {
		t.Fatalf("sendDirect exited %d\n%s", code, e.logs.String())
	}
	if got := readFile(t, filepath.Join(e.localInbox, "inbound", "from-remote.txt")); got != "sent from devlabs" {
		t.Fatalf("content: %q", got)
	}
	recs, _, _ := e.hub.Log(0)
	if len(recs) != 1 || recs[0].Dir != transfer.DirRecv || recs[0].Status != transfer.StatusOK {
		t.Fatalf("log: %+v", recs)
	}
}

func TestReceiveAsksBeforeSaving(t *testing.T) {
	e := newE2E(t, config.TransferConfig{}) // ask is the default
	if err := e.hub.SetReceive(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "token installed", func() bool { return e.remoteToken() != "" })

	send := func(name string) chan error {
		done := make(chan error, 1)
		go func() {
			conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(e.remotePort)))
			if err != nil {
				done <- err
				return
			}
			f := transfer.File{FileInfo: transfer.FileInfo{Name: name, Size: 4}, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("data")), nil }}
			_, err = transfer.Send(e.ctx(), conn, []transfer.File{f}, transfer.SendOptions{Token: e.remoteToken()})
			done <- err
		}()
		return done
	}
	waitPending := func() transfer.Pending {
		var p []transfer.Pending
		eventually(t, "offer waiting", func() bool { p = e.hub.Pending(); return len(p) == 1 })
		return p[0]
	}

	done := send("wanted.txt")
	p := waitPending()
	if n := e.notes.String(); !strings.Contains(n, "devlabs wants to send wanted.txt (4 B)") || !strings.Contains(n, "mole inbox accept") {
		t.Fatalf("the user was not told: %q", n)
	}
	if p.Peer != "devlabs" || p.Total != 4 || len(p.Files) != 1 || p.Files[0].Name != "wanted.txt" {
		t.Fatalf("offer: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(e.localInbox, "wanted.txt")); err == nil {
		t.Fatal("saved before the user answered")
	}
	if err := e.hub.Decide(p.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("accepted transfer failed: %v", err)
	}
	if readFile(t, filepath.Join(e.localInbox, "wanted.txt")) != "data" {
		t.Fatal("accepted file missing")
	}

	done = send("unwanted.txt")
	p = waitPending()
	if err := e.hub.Decide(p.ID, false); err != nil {
		t.Fatal(err)
	}
	var re *transfer.RemoteError
	if err := <-done; !errors.As(err, &re) {
		t.Fatalf("rejection should reach the sender: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.localInbox, "unwanted.txt")); err == nil {
		t.Fatal("a rejected file was saved")
	}
	recs, _, _ := e.hub.Log(0)
	var rejected int
	for _, r := range recs {
		if r.Status == transfer.StatusRejected {
			rejected++
		}
	}
	if rejected != 1 {
		t.Fatalf("log: %+v", recs)
	}
}

func TestWrongTokenIsRefused(t *testing.T) {
	e := newE2E(t, config.TransferConfig{Accept: "auto"})
	if err := e.hub.SetReceive(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "token installed", func() bool { return e.remoteToken() != "" })
	other := strings.Repeat("ab", 32)
	if err := transfer.WriteToken(transfer.TokenPath(e.remoteState), other); err != nil {
		t.Fatal(err)
	}
	e.onRemote()
	f, _ := transfer.FileFromPath(writeTemp(t, "sneaky.txt", []byte("x")))
	if code := sendDirect(e.ctx(), config.Default(), []transfer.File{f}, ""); code == 0 {
		t.Fatal("a wrong token was accepted")
	}
	if ents, _ := os.ReadDir(e.localInbox); len(ents) != 0 {
		t.Fatalf("something was written: %v", ents)
	}
}

func TestReceiveOffClosesEverything(t *testing.T) {
	e := newE2E(t, config.TransferConfig{Accept: "auto"})
	if err := e.hub.SetReceive(true); err != nil {
		t.Fatal(err)
	}
	remote := net.JoinHostPort("127.0.0.1", strconv.Itoa(e.remotePort))
	eventually(t, "remote port open", func() bool {
		c, err := net.DialTimeout("tcp", remote, 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	if on, _ := e.hub.Info()["receive"].(bool); !on {
		t.Fatal("Info does not report receiving")
	}
	if err := e.hub.SetReceive(false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "remote port closed", func() bool {
		c, err := net.DialTimeout("tcp", remote, 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
	if on, _ := e.hub.Info()["receive"].(bool); on {
		t.Fatal("Info still reports receiving")
	}
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(e.hub.settings().PortOrDefault()))
	if c, err := net.DialTimeout("tcp", local, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("the local receiver is still listening")
	}
	if err := e.hub.SetReceive(false); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := e.hub.SetReceive(true); err != nil { // and can come back
		t.Fatalf("re-enable: %v", err)
	}
}

func TestReceiveSurvivesReconnect(t *testing.T) {
	e := newE2E(t, config.TransferConfig{Accept: "auto"})
	if err := e.hub.SetReceive(true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "token installed", func() bool { return e.remoteToken() != "" })
	if err := os.Remove(transfer.TokenPath(e.remoteState)); err != nil {
		t.Fatal(err)
	}

	e.srv.kill()
	eventually(t, "token reinstalled after the reconnect", func() bool { return e.remoteToken() != "" })

	e.onRemote()
	f, _ := transfer.FileFromPath(writeTemp(t, "after.txt", []byte("still works")))
	eventually(t, "a transfer works after the reconnect", func() bool {
		return sendDirect(e.ctx(), config.Default(), []transfer.File{f}, "") == 0
	})
	if len(e.pf.ListPush()) != 0 {
		t.Fatal("the hidden forward became visible")
	}
}

func TestReceiveFailsCleanly(t *testing.T) {
	// The local port is taken: turning receive on fails and leaves nothing behind.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	e := newE2E(t, config.TransferConfig{Port: busy.Addr().(*net.TCPAddr).Port})
	if err := e.hub.SetReceive(true); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("got %v", err)
	}
	if on, _ := e.hub.Info()["receive"].(bool); on {
		t.Fatal("reports receiving after a failed start")
	}

	// A user push already owns the port: conflict, and the listener is released.
	e2 := newE2E(t, config.TransferConfig{})
	port := e2.hub.settings().PortOrDefault()
	if err := e2.pf.AddPush(port, freeLoopbackPort(t), ""); err != nil {
		t.Fatal(err)
	}
	if err := e2.hub.SetReceive(true); err == nil || !strings.Contains(err.Error(), "already pushed") {
		t.Fatalf("got %v", err)
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond); err == nil {
		// The user's own push forward listens on that port, so a connection
		// succeeds; what must not exist is a second (receiver) listener.
		c.Close()
	}
	if infos := e2.pf.ListPush(); len(infos) != 1 || infos[0].Local != port {
		t.Fatalf("the user's push was disturbed: %+v", infos)
	}
}

func TestRecvSetTokenValidates(t *testing.T) {
	dir := t.TempDir()
	if err := recvSetToken(strings.NewReader("not-a-token\n"), dir); err == nil {
		t.Fatal("invalid token accepted")
	}
	tok := strings.Repeat("0f", 32)
	if err := recvSetToken(strings.NewReader(tok+"\nnope\n"), dir); err == nil {
		t.Fatal("invalid port accepted")
	}
	if err := recvSetToken(strings.NewReader(tok+"\n9100\n"), dir); err != nil {
		t.Fatal(err)
	}
	if got, err := transfer.ReadToken(transfer.TokenPath(dir)); err != nil || got != tok {
		t.Fatalf("%q %v", got, err)
	}
	if p, err := transfer.ReadPeerPort(transfer.PeerPortPath(dir)); err != nil || p != 9100 {
		t.Fatalf("%d %v", p, err)
	}
}

func TestChooseRoute(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	cfg := config.Default()
	if _, err := chooseRoute(cfg, false); err == nil {
		t.Fatal("no daemon and no token should be an error")
	}
	cfg.Remote = "devlabs"
	if _, err := chooseRoute(cfg, false); err == nil || !strings.Contains(err.Error(), "admin_addr") {
		t.Fatalf("without admin_addr: %v", err)
	}
	cfg.AdminAddr = "127.0.0.1:1" // nothing listens there
	if _, err := chooseRoute(cfg, false); err == nil || !strings.Contains(err.Error(), "mole up") {
		t.Fatalf("daemon down: %v", err)
	}
	if r, err := chooseRoute(cfg, true); err != nil || r != routeDirect {
		t.Fatalf("-direct: %v %v", r, err)
	}
	if err := transfer.WriteToken(transfer.TokenPath(config.StateDir()), strings.Repeat("0f", 32)); err != nil {
		t.Fatal(err)
	}
	if r, err := chooseRoute(cfg, false); err != nil || r != routeDirect {
		t.Fatalf("with a token: %v %v", r, err)
	}
	api := httptest.NewServer(admin.New(admin.NewStats(), nil).Handler())
	defer api.Close()
	cfg.AdminAddr = strings.TrimPrefix(api.URL, "http://")
	if r, err := chooseRoute(cfg, false); err != nil || r != routeDaemon {
		t.Fatalf("with a live daemon: %v %v", r, err)
	}
}

func TestHumanSize(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestOfferSummary(t *testing.T) {
	cases := []struct {
		names []string
		total int64
		want  string
	}{
		{[]string{"a.txt"}, 5, "a.txt (5 B)"},
		{[]string{"a", "b"}, 2048, "a, b (2.0 KiB)"},
		{[]string{"a", "b", "c", "d"}, 3 << 20, "a, b and 2 more (3.0 MiB)"},
	}
	for _, c := range cases {
		if got := offerSummary(c.names, c.total); got != c.want {
			t.Errorf("offerSummary(%v) = %q, want %q", c.names, got, c.want)
		}
	}
	long := offerSummary([]string{strings.Repeat("x", 300)}, 1)
	if len([]rune(long)) > 120 {
		t.Errorf("summary not truncated: %d runes", len([]rune(long)))
	}
}
