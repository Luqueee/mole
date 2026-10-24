package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/proxy"
	"github.com/Luqueee/mole/internal/tunnel"
	"golang.org/x/crypto/ssh"
)

// sshTestServer is a minimal in-process sshd: key auth, sessions, and
// tcpip-forward/cancel-tcpip-forward with forwarded-tcpip channels.
type sshTestServer struct {
	ln  net.Listener
	cfg *ssh.ServerConfig
	mu  sync.Mutex
	scs []*ssh.ServerConn
	// backend is where direct-tcpip channels are connected (the requested
	// host is not resolved here); hosts records what clients asked for.
	backend string
	hosts   []string
	// stall makes the server authenticate and then ignore everything.
	stall bool
	// ignoreCancel accepts tcpip-forward but never answers its cancellation.
	ignoreCancel bool
	// exec, when set, runs "exec" session requests: ch carries the command's
	// stdin/stdout (and ch.Stderr() its stderr); the result is its exit status.
	exec func(cmd string, ch ssh.Channel) uint32
}

func (s *sshTestServer) session(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	for r := range reqs {
		if r.Type != "exec" {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
			continue
		}
		var p struct{ Command string }
		_ = ssh.Unmarshal(r.Payload, &p)
		_ = r.Reply(true, nil)
		go func() {
			status := s.exec(p.Command, ch)
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			_ = ch.Close()
		}()
	}
}

func newSSHTestServer(t *testing.T, clientKey ssh.PublicKey) *sshTestServer {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(clientKey.Marshal()) {
			return nil, nil
		}
		return nil, io.ErrUnexpectedEOF
	}}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sshTestServer{ln: ln, cfg: cfg}
	t.Cleanup(func() { ln.Close(); s.kill() })
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(raw)
		}
	}()
	return s
}

func (s *sshTestServer) direct(nc ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigAddr string
		OrigPort uint32
	}
	_ = ssh.Unmarshal(nc.ExtraData(), &p)
	s.mu.Lock()
	s.hosts = append(s.hosts, net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
	backend := s.backend
	s.mu.Unlock()
	c, err := net.Dial("tcp", backend)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { _, _ = io.Copy(ch, c); _ = ch.CloseWrite() }()
	_, _ = io.Copy(c, ch)
	c.Close()
}

// kill drops every established SSH transport (the listener keeps accepting).
func (s *sshTestServer) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sc := range s.scs {
		_ = sc.Close()
	}
	s.scs = nil
}

func (s *sshTestServer) handle(raw net.Conn) {
	sc, chans, reqs, err := ssh.NewServerConn(raw, s.cfg)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.scs = append(s.scs, sc)
	s.mu.Unlock()
	if s.stall {
		// authenticated, then never answers any channel open or request
		go func() {
			for range chans {
			}
		}()
		for range reqs {
		}
		return
	}
	go func() {
		for nc := range chans {
			if nc.ChannelType() == "direct-tcpip" {
				go s.direct(nc)
				continue
			}
			if nc.ChannelType() == "session" && s.exec != nil {
				go s.session(nc)
				continue
			}
			ch, r, err := nc.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(r)
			_ = ch
		}
	}()
	lns := map[uint32]net.Listener{}
	defer func() {
		for _, l := range lns {
			_ = l.Close()
		}
	}()
	for r := range reqs {
		var p struct {
			Addr string
			Port uint32
		}
		switch r.Type {
		case "tcpip-forward":
			_ = ssh.Unmarshal(r.Payload, &p)
			l, err := net.Listen("tcp", net.JoinHostPort(p.Addr, strconv.Itoa(int(p.Port))))
			if err != nil {
				_ = r.Reply(false, nil)
				continue
			}
			var reply []byte
			if p.Port == 0 { // client asked for any port: tell it which
				p.Port = uint32(l.Addr().(*net.TCPAddr).Port)
				reply = ssh.Marshal(struct{ Port uint32 }{p.Port})
			}
			lns[p.Port] = l
			_ = r.Reply(true, reply)
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					go func() {
						ch, creqs, err := sc.OpenChannel("forwarded-tcpip", ssh.Marshal(struct {
							Addr       string
							Port       uint32
							OriginAddr string
							OriginPort uint32
						}{p.Addr, p.Port, "127.0.0.1", 1}))
						if err != nil {
							c.Close()
							return
						}
						go ssh.DiscardRequests(creqs)
						go func() { _, _ = io.Copy(ch, c); _ = ch.CloseWrite() }()
						_, _ = io.Copy(c, ch)
						c.Close()
					}()
				}
			}()
		case "cancel-tcpip-forward":
			_ = ssh.Unmarshal(r.Payload, &p)
			if s.ignoreCancel {
				continue // accepted the forward, never answers the cancel
			}
			if l := lns[p.Port]; l != nil {
				_ = l.Close()
				delete(lns, p.Port)
			}
			_ = r.Reply(true, nil)
		default:
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}
}

type syncBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}
func (b *syncBuf) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.sb.String() }

// remotePing dials the remote-side listener and returns what the local
// service answered, or "" if nothing reachable answered.
func remotePing(port int) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	b, err := io.ReadAll(c)
	if err != nil { // EOF must propagate; a timeout means half-close was lost
		return ""
	}
	return string(b)
}

func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Real SSH transport: killing it must be healed by Watch alone (no manual
// reopenAll), and neither reconnect nor unpush may leave accept-failed loops.
func TestPushOverRealSSHSurvivesWatchReconnect(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = pub
	srv := newSSHTestServer(t, signer.PublicKey())

	buf := &syncBuf{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr, err := tunnel.New(tunnel.Remote{User: "u", Addr: srv.ln.Addr().String(), IdentityFiles: []string{keyFile}}, true, log)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Watch(ctx, 50*time.Millisecond)

	local := localEcho(t)
	remotePort := freeLoopbackPort(t)
	pf := newPushForwarder(mgr, log, proxy.Hooks{})
	defer pf.closeAll()
	mgr.OnReconnect(pf.reopenAll)
	pf.start([]config.PushRule{{Local: local, Remote: remotePort}})

	if got := remotePing(remotePort); got != "pong" {
		t.Fatalf("before kill: %q", got)
	}

	srv.kill()
	eventually(t, "push forward restored by Watch", func() bool { return remotePing(remotePort) == "pong" })

	if err := pf.RemovePush(local); err != nil {
		t.Fatal(err)
	}
	eventually(t, "remote listener closed by unpush", func() bool { return remotePing(remotePort) == "" })

	time.Sleep(300 * time.Millisecond)
	if strings.Contains(buf.String(), "accept failed") {
		t.Fatalf("accept loop spinning after close:\n%s", buf.String())
	}
}
