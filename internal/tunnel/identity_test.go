package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testIdentity is a key pair that tests write to files and/or load into a
// test agent.
type testIdentity struct {
	private ed25519.PrivateKey
	public  ssh.PublicKey
}

func newTestIdentity(t *testing.T) testIdentity {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	return testIdentity{private: private, public: public}
}

// writeFile stores the private key in OpenSSH format at path, encrypted
// when passphrase is non-empty.
func (id testIdentity) writeFile(t *testing.T, path, passphrase string) string {
	t.Helper()
	var block *pem.Block
	var err error
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(id.private, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(id.private, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// useTestAgent points the agent dialer at an in-memory agent holding ids.
// With no ids the agent is unavailable.
func useTestAgent(t *testing.T, ids ...testIdentity) *trackingConn {
	t.Helper()
	t.Cleanup(func() { dialAgentFunc = dialAgent })
	if len(ids) == 0 {
		dialAgentFunc = func(string) (net.Conn, error) { return nil, errors.New("no agent") }
		return nil
	}
	keyring := agent.NewKeyring()
	for _, id := range ids {
		if err := keyring.Add(agent.AddedKey{PrivateKey: id.private}); err != nil {
			t.Fatal(err)
		}
	}
	conn := newAgentConnection(t, keyring)
	dialAgentFunc = func(string) (net.Conn, error) { return conn, nil }
	return conn
}

func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func bufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Regression: x/crypto/ssh tries each AuthMethod name once, so with one
// PublicKeys method per key a rejected first key meant the rest (including
// every agent key) were never offered. The server here accepts only the
// last key, so the handshake succeeds only if every key is offered.
func TestNew_TriesKeysAfterTheFirstIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home string, rejected, accepted testIdentity) Remote
	}{
		{"second identity file", func(t *testing.T, home string, rejected, accepted testIdentity) Remote {
			useTestAgent(t)
			return Remote{IdentityFiles: []string{
				rejected.writeFile(t, filepath.Join(home, "first"), ""),
				accepted.writeFile(t, filepath.Join(home, "second"), ""),
			}}
		}},
		{"agent key after identity file", func(t *testing.T, home string, rejected, accepted testIdentity) Remote {
			useTestAgent(t, accepted)
			return Remote{IdentityFiles: []string{rejected.writeFile(t, filepath.Join(home, "first"), "")}}
		}},
		{"default key after agent key", func(t *testing.T, home string, rejected, accepted testIdentity) Remote {
			useTestAgent(t, rejected)
			accepted.writeFile(t, filepath.Join(home, ".ssh", "id_rsa"), "")
			return Remote{}
		}},
		{"later default key", func(t *testing.T, home string, rejected, accepted testIdentity) Remote {
			useTestAgent(t)
			rejected.writeFile(t, filepath.Join(home, ".ssh", "id_ed25519"), "")
			accepted.writeFile(t, filepath.Join(home, ".ssh", "id_ecdsa"), "")
			return Remote{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := tempHome(t)
			rejected, accepted := newTestIdentity(t), newTestIdentity(t)
			remote := tc.setup(t, home, rejected, accepted)
			remote.User = "test"
			remote.Addr = agentSSHServer(t, accepted.public)
			manager, err := New(remote, true, discardLogger())
			if err != nil {
				t.Fatalf("authorized key was not offered after a rejected one: %v", err)
			}
			_ = manager.Close()
		})
	}
}

func signerKeys(signers []ssh.Signer) []string {
	var keys []string
	for _, s := range signers {
		keys = append(keys, ssh.FingerprintSHA256(s.PublicKey()))
	}
	return keys
}

func wantKeys(ids ...testIdentity) []string {
	var keys []string
	for _, id := range ids {
		keys = append(keys, ssh.FingerprintSHA256(id.public))
	}
	return keys
}

func TestIdentitySigners_OrderDedupeAndEncryptedKeys(t *testing.T) {
	home := tempHome(t)
	explicit, inAgent, encryptedInAgent, encrypted, agentOnly, def :=
		newTestIdentity(t), newTestIdentity(t), newTestIdentity(t), newTestIdentity(t), newTestIdentity(t), newTestIdentity(t)
	conn := useTestAgent(t, agentOnly, inAgent, encryptedInAgent)
	def.writeFile(t, filepath.Join(home, ".ssh", "id_ed25519"), "")
	encryptedPath := encrypted.writeFile(t, filepath.Join(home, "locked"), "secret")
	encryptedInAgentPath := encryptedInAgent.writeFile(t, filepath.Join(home, "unlocked-by-agent"), "secret")
	remote := Remote{IdentityFiles: []string{
		explicit.writeFile(t, filepath.Join(home, "explicit"), ""),
		filepath.Join(home, "missing"),
		encryptedInAgentPath,
		encryptedPath,
		inAgent.writeFile(t, filepath.Join(home, "in-agent"), ""),
	}}

	var logs bytes.Buffer
	signers, agentConn, err := identitySigners(remote, bufferLogger(&logs))
	if err != nil {
		t.Fatal(err)
	}
	// Files first (the encrypted one through its agent copy, the locked
	// one skipped), then the remaining agent key, then defaults. Keys held
	// by both a file and the agent are offered once.
	if got, want := signerKeys(signers), wantKeys(explicit, encryptedInAgent, inAgent, agentOnly, def); !equalStrings(got, want) {
		t.Fatalf("signer order = %v, want %v", got, want)
	}
	if agentConn == nil || conn.closed.Load() {
		t.Fatal("agent connection must stay open for agent-backed signers")
	}
	out := logs.String()
	if !strings.Contains(out, encryptedPath) || !strings.Contains(out, "ssh-add") {
		t.Fatalf("encrypted key skipped without an ssh-add hint; logs:\n%s", out)
	}
	if strings.Contains(out, encryptedInAgentPath) {
		t.Fatalf("warned about an encrypted key the agent provides; logs:\n%s", out)
	}
	if strings.Contains(out, "missing") {
		t.Fatalf("logged a missing key file; logs:\n%s", out)
	}
}

func TestIdentitySigners_SSHDefaultIdentityFilesComeAfterAgent(t *testing.T) {
	home := tempHome(t)
	rsa, ed, agentKey := newTestIdentity(t), newTestIdentity(t), newTestIdentity(t)
	useTestAgent(t, agentKey)
	ssh := filepath.Join(home, ".ssh")
	// ssh -G prints OpenSSH's default list when ssh_config sets none.
	remote := Remote{IdentityFiles: []string{
		rsa.writeFile(t, filepath.Join(ssh, "id_rsa"), ""),
		filepath.Join(ssh, "id_ecdsa"),
		filepath.Join(ssh, "id_ecdsa_sk"),
		ed.writeFile(t, filepath.Join(ssh, "id_ed25519"), ""),
		filepath.Join(ssh, "id_ed25519_sk"),
	}}
	signers, _, err := identitySigners(remote, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := signerKeys(signers), wantKeys(agentKey, ed, rsa); !equalStrings(got, want) {
		t.Fatalf("signer order = %v, want %v", got, want)
	}
}

func TestIdentitySigners_IdentitiesOnly(t *testing.T) {
	t.Run("agent keys not in IdentityFile are not offered", func(t *testing.T) {
		home := tempHome(t)
		file, agentKey, def := newTestIdentity(t), newTestIdentity(t), newTestIdentity(t)
		conn := useTestAgent(t, agentKey)
		def.writeFile(t, filepath.Join(home, ".ssh", "id_ed25519"), "")
		remote := Remote{
			IdentitiesOnly: true,
			IdentityFiles:  []string{file.writeFile(t, filepath.Join(home, "key"), "")},
		}
		signers, agentConn, err := identitySigners(remote, discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		if got, want := signerKeys(signers), wantKeys(file); !equalStrings(got, want) {
			t.Fatalf("signers = %v, want only the identity file %v", got, want)
		}
		if agentConn != nil || !conn.closed.Load() {
			t.Fatal("unused agent connection was not closed")
		}
	})
	t.Run("agent copy of an encrypted IdentityFile is offered", func(t *testing.T) {
		home := tempHome(t)
		locked, other := newTestIdentity(t), newTestIdentity(t)
		conn := useTestAgent(t, other, locked)
		remote := Remote{
			IdentitiesOnly: true,
			IdentityFiles:  []string{locked.writeFile(t, filepath.Join(home, "key"), "secret")},
		}
		signers, agentConn, err := identitySigners(remote, discardLogger())
		if err != nil {
			t.Fatal(err)
		}
		if got, want := signerKeys(signers), wantKeys(locked); !equalStrings(got, want) {
			t.Fatalf("signers = %v, want %v", got, want)
		}
		if agentConn == nil || conn.closed.Load() {
			t.Fatal("agent connection closed although an agent signer is used")
		}
	})
}

func TestIdentitySigners_OnlyEncryptedKeysExplainsSSHAdd(t *testing.T) {
	home := tempHome(t)
	useTestAgent(t)
	path := newTestIdentity(t).writeFile(t, filepath.Join(home, ".ssh", "id_ed25519"), "secret")
	_, _, err := identitySigners(Remote{}, discardLogger())
	if err == nil {
		t.Fatal("identitySigners succeeded without a usable key")
	}
	// cmd/mole's doctor classifies this error by its prefix.
	if msg := err.Error(); !strings.HasPrefix(msg, "no SSH auth methods") || !strings.Contains(msg, "ssh-add") || !strings.Contains(msg, path) {
		t.Fatalf("error = %q, want the no-auth prefix and an ssh-add hint naming %s", msg, path)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// freshAgents makes every agent dial return a new connection to one
// keyring, as separate dials of a real agent socket would.
func freshAgents(t *testing.T, ids ...testIdentity) {
	t.Helper()
	keyring := agent.NewKeyring()
	for _, id := range ids {
		if err := keyring.Add(agent.AddedKey{PrivateKey: id.private}); err != nil {
			t.Fatal(err)
		}
	}
	dialAgentFunc = func(string) (net.Conn, error) { return newAgentConnection(t, keyring), nil }
	t.Cleanup(func() { dialAgentFunc = dialAgent })
}

// Each ProxyJump hop is verified with its own ssh_config host-key policy.
func TestNew_ProxyJumpHopUsesItsHostKeyPolicy(t *testing.T) {
	home := tempHome(t)
	id := newTestIdentity(t)
	freshAgents(t, id)
	jump := Remote{
		User:                  "test",
		Addr:                  agentSSHServer(t, id.public),
		StrictHostKeyChecking: "true",
		UserKnownHostsFiles:   []string{filepath.Join(home, "jump_known_hosts")},
	}
	target := Remote{User: "test", Addr: agentSSHServer(t, id.public), ProxyJumps: []Remote{jump}}
	manager, err := New(target, false, discardLogger())
	if err == nil {
		_ = manager.Close()
		t.Fatal("connected through a jump host whose unknown key StrictHostKeyChecking must reject")
	}
	if !strings.Contains(err.Error(), "StrictHostKeyChecking") || !strings.Contains(err.Error(), knownhosts.Normalize(jump.Addr)) {
		t.Fatalf("error = %v, want the strict rejection of the jump host", err)
	}
}

func TestNew_WarnsAboutUnsupportedDirectives(t *testing.T) {
	tempHome(t)
	id := newTestIdentity(t)
	freshAgents(t, id)
	jump := Remote{User: "test", Addr: agentSSHServer(t, id.public), CertificateFiles: []string{"/keys/jump-cert.pub"}}
	target := Remote{User: "test", Addr: agentSSHServer(t, id.public), ProxyCommand: "nc %h %p", ProxyJumps: []Remote{jump}}
	var logs bytes.Buffer
	manager, err := New(target, true, bufferLogger(&logs))
	if err != nil {
		t.Fatal(err)
	}
	_ = manager.Close()
	out := logs.String()
	for _, want := range []string{"ProxyCommand is not supported", "nc %h %p", "CertificateFile is not supported", "/keys/jump-cert.pub"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs do not mention %q:\n%s", want, out)
		}
	}
}
