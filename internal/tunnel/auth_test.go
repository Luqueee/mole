package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type trackingConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *trackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

type testAgent struct {
	agent.Agent
	listErr error
}

func (a *testAgent) List() ([]*agent.Key, error) {
	if a.listErr != nil {
		return nil, a.listErr
	}
	return nil, nil
}

func newAgentConnection(t *testing.T, srv agent.Agent) *trackingConn {
	t.Helper()

	client, server := net.Pipe()
	tracked := &trackingConn{Conn: client}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.ServeAgent(srv, server)
		_ = server.Close()
	}()
	t.Cleanup(func() {
		_ = tracked.Close()
		<-done
	})
	return tracked
}

func configureTestAgent(t *testing.T, conn net.Conn) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dialAgentFunc = func(string) (net.Conn, error) {
		return conn, nil
	}
	t.Cleanup(func() {
		dialAgentFunc = dialAgent
	})
}

func TestBuildSSHConfig_ClosesAgentAfterSignersFailure(t *testing.T) {
	conn := newAgentConnection(t, &testAgent{
		Agent:   agent.NewKeyring(),
		listErr: errors.New("agent unavailable"),
	})
	configureTestAgent(t, conn)

	_, _, err := buildSSHConfig(Remote{User: "root", Addr: "unused:22"}, true, discardLogger())
	if err == nil {
		t.Fatal("buildSSHConfig returned nil error, want no auth methods")
	}
	if !conn.closed.Load() {
		t.Fatal("agent connection remained open after Signers failure")
	}
}

func TestBuildSSHConfig_ClosesAgentAfterEmptySigners(t *testing.T) {
	conn := newAgentConnection(t, &testAgent{Agent: agent.NewKeyring()})
	configureTestAgent(t, conn)

	_, _, err := buildSSHConfig(Remote{User: "root", Addr: "unused:22"}, true, discardLogger())
	if err == nil {
		t.Fatal("buildSSHConfig returned nil error, want no auth methods")
	}
	if !conn.closed.Load() {
		t.Fatal("agent connection remained open after empty Signers result")
	}
}

func TestBuildSSHConfig_RetainsAgentForSuccessfulSigner(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatalf("add test key to agent: %v", err)
	}
	conn := newAgentConnection(t, keyring)
	configureTestAgent(t, conn)

	_, _, err = buildSSHConfig(Remote{User: "root", Addr: "unused:22"}, true, discardLogger())
	if err != nil {
		t.Fatalf("buildSSHConfig returned error: %v", err)
	}
	if conn.closed.Load() {
		t.Fatal("agent connection closed despite successful signer")
	}
}

func TestManagerClose_ClosesRetainedAgentConnection(t *testing.T) {
	conn := newAgentConnection(t, &testAgent{Agent: agent.NewKeyring()})
	m := &Manager{
		client:    &liveConn{},
		agentConn: conn,
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Manager.Close returned error: %v", err)
	}
	if !conn.closed.Load() {
		t.Fatal("Manager.Close left the retained agent connection open")
	}
}

func agentKey(t *testing.T) (agent.Agent, ssh.PublicKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	return keyring, public
}

// Each server requires its own key; a jump accepts direct-tcpip channels so the
// target must authenticate independently over the real SSH transport.
func agentSSHServer(t *testing.T, public ssh.PublicKey) string {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), public.Marshal()) {
				return nil, fmt.Errorf("wrong agent key")
			}
			return nil, nil
		}}
		cfg.AddHostKey(signer)
		conn, channels, requests, err := ssh.NewServerConn(raw, cfg)
		if err != nil {
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		for incoming := range channels {
			if incoming.ChannelType() != "direct-tcpip" {
				_ = incoming.Reject(ssh.UnknownChannelType, "direct-tcpip only")
				continue
			}
			go func(incoming ssh.NewChannel) {
				var target struct {
					Host       string
					Port       uint32
					OriginHost string
					OriginPort uint32
				}
				if err := ssh.Unmarshal(incoming.ExtraData(), &target); err != nil {
					_ = incoming.Reject(ssh.ConnectionFailed, err.Error())
					return
				}
				dst, err := net.DialTimeout("tcp", net.JoinHostPort(target.Host, fmt.Sprint(target.Port)), time.Second)
				if err != nil {
					_ = incoming.Reject(ssh.ConnectionFailed, err.Error())
					return
				}
				defer dst.Close()
				ch, reqs, err := incoming.Accept()
				if err != nil {
					return
				}
				defer ch.Close()
				go ssh.DiscardRequests(reqs)
				go func() {
					_, _ = io.Copy(dst, ch)
					_ = dst.Close()
				}()
				_, _ = io.Copy(ch, dst)
			}(incoming)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("SSH agent test server did not stop")
		}
	})
	return ln.Addr().String()
}

func TestIdentityAgentForAliasAndProxyJump(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("alias fixture uses a POSIX ssh executable")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	targetRing, targetKey := agentKey(t)
	jumpRing, jumpKey := agentKey(t)
	targetAddr := agentSSHServer(t, targetKey)
	jumpAddr := agentSSHServer(t, jumpKey)
	targetAgent := newAgentConnection(t, targetRing)
	jumpAgent := newAgentConnection(t, jumpRing)
	dialAgentFunc = func(sock string) (net.Conn, error) {
		switch sock {
		case filepath.Join(home, "target.sock"):
			return targetAgent, nil
		case filepath.Join(home, "jump.sock"):
			return jumpAgent, nil
		}
		return nil, fmt.Errorf("no agent at %q", sock)
	}
	t.Cleanup(func() { dialAgentFunc = dialAgent })
	_, targetPort, _ := net.SplitHostPort(targetAddr)
	_, jumpPort, _ := net.SplitHostPort(jumpAddr)
	fixture := fmt.Sprintf(`#!/bin/sh
case "$2" in
  target) printf 'user test\nhostname 127.0.0.1\nport %s\nidentityagent %s/target.sock\nproxyjump jump\n' ;;
  jump) printf 'user test\nhostname 127.0.0.1\nport %s\nidentityagent %s/jump.sock\nproxyjump none\n' ;;
  *) exit 1 ;;
esac
`, targetPort, home, jumpPort, home)
	if err := os.WriteFile(filepath.Join(home, "ssh"), []byte(fixture), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home)
	remote, err := ResolveRemote("target", 22)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(remote, true, discardLogger())
	if err != nil {
		t.Fatalf("configured agents must authenticate without SSH_AUTH_SOCK: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
}

func TestIdentityAgentSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MOLE_TEST_AGENT", "configured")
	t.Setenv("MOLE_UNSET_TEST_AGENT", "")
	for _, tc := range []struct {
		name, configured, inherited, available string
		wantError                              bool
	}{
		{"environment fallback", "", "inherited", "inherited", false},
		{"configured overrides environment", "configured", "inherited", "configured", false},
		{"configured without environment", "configured", "", "configured", false},
		{"unavailable override does not fall back", "missing", "inherited", "inherited", true},
		{"none disables inherited agent", "none", "inherited", "inherited", true},
		{"explicit environment", "SSH_AUTH_SOCK", "inherited", "inherited", false},
		{"empty explicit environment", "SSH_AUTH_SOCK", "", "", true},
		{"environment variable", "$MOLE_TEST_AGENT", "inherited", "configured", false},
		{"empty environment variable", "${MOLE_UNSET_TEST_AGENT}", "inherited", "", true},
		{"home expansion", "~/agent.sock", "inherited", filepath.Join(home, "agent.sock"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SSH_AUTH_SOCK", tc.inherited)
			ring, public := agentKey(t)
			conn := newAgentConnection(t, ring)
			dialAgentFunc = func(sock string) (net.Conn, error) {
				if sock == tc.available {
					return conn, nil
				}
				return nil, fmt.Errorf("agent unavailable at %q", sock)
			}
			t.Cleanup(func() { dialAgentFunc = dialAgent })
			remote := Remote{User: "test", Addr: agentSSHServer(t, public), IdentityAgent: tc.configured}
			manager, err := New(remote, true, discardLogger())
			if manager != nil {
				t.Cleanup(func() { _ = manager.Close() })
			}
			if tc.wantError {
				if err == nil {
					t.Fatal("authenticated using an agent the configuration must not select")
				}
			} else if err != nil {
				t.Fatalf("configured agent could not authenticate: %v", err)
			}
		})
	}
}
