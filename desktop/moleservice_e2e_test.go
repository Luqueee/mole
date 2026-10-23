package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Luqueee/mole/internal/config"
	"golang.org/x/crypto/ssh"
)

// pushSSHServer is an in-process sshd that only knows `tcpip-forward`: it
// listens where the client asks and hands every accepted connection back as
// a forwarded-tcpip channel, which is what a real sshd does for `ssh -R`.
func pushSSHServer(t *testing.T, clientKey ssh.PublicKey) string {
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
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(raw, cfg)
		}
	}()
	return ln.Addr().String()
}

func serveConn(raw net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go func() {
		for nc := range chans {
			_ = nc.Reject(ssh.UnknownChannelType, "push test server")
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
		_ = ssh.Unmarshal(r.Payload, &p)
		switch r.Type {
		case "tcpip-forward":
			l, err := net.Listen("tcp", net.JoinHostPort(p.Addr, strconv.Itoa(int(p.Port))))
			if err != nil {
				_ = r.Reply(false, nil)
				continue
			}
			lns[p.Port] = l
			_ = r.Reply(true, nil)
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

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The desktop service drives the real `mole` CLI against a real daemon: a
// push added through AddPush opens the remote listener, carries bytes from
// the remote back to the local service, shows up in Status and as a
// dir=push event on the local port, and RemovePush closes it again.
func TestServicePushEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the mole daemon")
	}
	// Build with the real toolchain environment, before HOME is redirected.
	bin := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "mole"), "github.com/Luqueee/mole/cmd/mole").CombinedOutput(); err != nil {
		t.Fatalf("build mole: %v\n%s", err, out)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Chdir(home) // config.LocalPath is relative to the working directory

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshAddr := pushSSHServer(t, signer.PublicKey())

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(config.StateDir(), "mole.log"))
			t.Logf("daemon log:\n%s", b)
		}
	})

	adminPort := freePort(t)
	cfg := fmt.Sprintf("remote: u@%s\nauto_discover: false\nadmin_addr: 127.0.0.1:%d\n", sshAddr, adminPort)
	if err := os.WriteFile(config.LocalPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// the local service being pushed: answers "pong:<request>"
	svcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer svcLn.Close()
	local := svcLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := svcLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				_, _ = c.Write(append([]byte("pong:"), b...))
			}()
		}
	}()
	remote := freePort(t)

	m := NewMoleService()
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	waitFor(t, "daemon up", func() bool { return m.GetStatus().Running })

	if err := m.AddPush(local, remote); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "push active in Status", func() bool {
		p := m.GetStatus().Push
		return len(p) == 1 && p[0].Local == local && p[0].Remote == remote && p[0].State == "active"
	})

	// a client on the "remote" side reaches the local service through the SSH connection
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(remote)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("ping"))
	_ = c.(*net.TCPConn).CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	c.Close()
	if err != nil || string(got) != "pong:ping" {
		t.Fatalf("pushed connection returned %q, %v", got, err)
	}

	var ev Event
	waitFor(t, "push event", func() bool {
		for _, e := range m.GetEvents(0).Events {
			if e.Dir == "push" {
				ev = e
				return true
			}
		}
		return false
	})
	if ev.Port != local || ev.BytesOut != 4 || ev.BytesIn != int64(len("pong:ping")) || ev.Failed {
		t.Fatalf("event = %+v, want push on local port %d with 4 out / 9 in", ev, local)
	}

	if err := m.RemovePush(local); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "push gone from Status", func() bool { return len(m.GetStatus().Push) == 0 })
	waitFor(t, "remote listener closed", func() bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(remote)), time.Second)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
}
