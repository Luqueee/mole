package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"sync/atomic"
	"testing"

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

	_, err := buildSSHConfig("root", nil, "unused:22", true, discardLogger())
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

	_, err := buildSSHConfig("root", nil, "unused:22", true, discardLogger())
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

	cfg, err := buildSSHConfig("root", nil, "unused:22", true, discardLogger())
	if err != nil {
		t.Fatalf("buildSSHConfig returned error: %v", err)
	}
	if len(cfg.Auth) != 1 {
		t.Fatalf("len(Auth) = %d, want 1", len(cfg.Auth))
	}
	if conn.closed.Load() {
		t.Fatal("agent connection closed despite successful signer")
	}
}
