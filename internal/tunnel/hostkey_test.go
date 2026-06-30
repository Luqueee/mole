package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// discardLogger silences the TOFU warning so test output stays clean
// while still exercising the real log call inside the callback.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testRemote is the resolved peer address handed to the callback. SSH's
// default port (22) is stripped by knownhosts.Normalize, so the stored
// form is the bare IP.
func testRemote() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}
}

// genKey returns the public half of a fresh ed25519 keypair as an
// ssh.PublicKey. Each call generates new random key material, so two
// calls yield two distinct keys — exactly what the mismatch ("attacker
// presents a different key") case needs.
func genKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new ssh signer from ed25519 key: %v", err)
	}
	return signer.PublicKey()
}

// newKHPath returns a known_hosts path inside a per-test temp dir. The
// file does not exist yet; the code under test is responsible for
// creating it.
func newKHPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "known_hosts")
}

const testHost = "workstation:22"

// 1. Trust on first use: an unknown host is ACCEPTED and its key is
// PERSISTED to known_hosts, so a later mismatch can be detected.
func TestKnownHostsCallback_TOFUAcceptsAndPersistsUnknownHost(t *testing.T) {
	khPath := newKHPath(t)
	keyA := genKey(t)

	cb, err := knownHostsCallback(khPath, discardLogger())
	if err != nil {
		t.Fatalf("knownHostsCallback() construction error: %v", err)
	}

	if err := cb(testHost, testRemote(), keyA); err != nil {
		t.Fatalf("first-contact with unknown host should be accepted (TOFU), got error: %v", err)
	}

	data, err := os.ReadFile(khPath)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("known_hosts is empty: the TOFU key was not persisted")
	}
	if !strings.Contains(string(data), "workstation") {
		t.Errorf("known_hosts does not contain the host; contents:\n%s", data)
	}
}

// 2. A host already in known_hosts whose presented key MATCHES the
// stored key is accepted.
func TestKnownHostsCallback_KnownHostMatchingKeyAccepted(t *testing.T) {
	khPath := newKHPath(t)
	keyA := genKey(t)

	// Seed the host directly so this case is independent of the TOFU path.
	if err := appendKnownHost(khPath, testHost, testRemote(), keyA); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}

	cb, err := knownHostsCallback(khPath, discardLogger())
	if err != nil {
		t.Fatalf("knownHostsCallback() construction error: %v", err)
	}

	if err := cb(testHost, testRemote(), keyA); err != nil {
		t.Fatalf("known host with matching key should be accepted, got error: %v", err)
	}
}

// 3. The core security guarantee: a host already in known_hosts whose
// presented key does NOT match the stored key is REJECTED, and the
// error names the mismatch. This is the MITM signal.
func TestKnownHostsCallback_MismatchedKeyRejected(t *testing.T) {
	khPath := newKHPath(t)
	keyA := genKey(t) // the key we trust
	keyB := genKey(t) // the key the impostor presents

	if err := appendKnownHost(khPath, testHost, testRemote(), keyA); err != nil {
		t.Fatalf("seed known_hosts with trusted key: %v", err)
	}

	cb, err := knownHostsCallback(khPath, discardLogger())
	if err != nil {
		t.Fatalf("knownHostsCallback() construction error: %v", err)
	}

	err = cb(testHost, testRemote(), keyB)
	if err == nil {
		t.Fatal("SECURITY: a key that does not match the stored host key was ACCEPTED; the MITM check is broken")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("rejection error should mention the mismatch, got: %v", err)
	}

	// Belt and braces: a rejected key must NOT be written to known_hosts.
	// If it were, a subsequent connect would silently accept the attacker.
	data, err := os.ReadFile(khPath)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if strings.Contains(string(data), serializeKey(keyB)) {
		t.Error("SECURITY: the mismatched (attacker) key was persisted to known_hosts")
	}
}

// 4. Reconnecting after TOFU must NOT append a second line. The callback
// re-reads known_hosts each handshake, so the second call sees the host
// as already known and matching, and leaves the file untouched.
func TestKnownHostsCallback_NoDuplicateAppendOnReconnect(t *testing.T) {
	khPath := newKHPath(t)
	keyA := genKey(t)

	cb, err := knownHostsCallback(khPath, discardLogger())
	if err != nil {
		t.Fatalf("knownHostsCallback() construction error: %v", err)
	}

	remote := testRemote()
	if err := cb(testHost, remote, keyA); err != nil {
		t.Fatalf("first connect (TOFU) should be accepted, got: %v", err)
	}
	if err := cb(testHost, remote, keyA); err != nil {
		t.Fatalf("reconnect with same key should be accepted, got: %v", err)
	}

	data, err := os.ReadFile(khPath)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	got := countHostLines(string(data), "workstation")
	if got != 1 {
		t.Errorf("known_hosts should hold exactly 1 line for the host after a reconnect, got %d; contents:\n%s", got, data)
	}
}

// 5. The --insecure escape hatch accepts any host with any key and never
// touches the filesystem. Verifies the legacy behaviour is preserved
// behind the explicit flag.
func TestHostKeyCallback_InsecureAcceptsAnything(t *testing.T) {
	cb, err := hostKeyCallback(true, discardLogger())
	if err != nil {
		t.Fatalf("hostKeyCallback(true) error: %v", err)
	}

	// A host never seen, with a freshly minted key: must still be accepted.
	if err := cb("never-seen-host:22", testRemote(), genKey(t)); err != nil {
		t.Errorf("insecure callback should accept any host/key, got error: %v", err)
	}
	// A second, different key for the same host: still accepted, since the
	// insecure callback performs no verification at all.
	if err := cb("never-seen-host:22", testRemote(), genKey(t)); err != nil {
		t.Errorf("insecure callback should accept a second differing key, got error: %v", err)
	}
}

// 6. A malformed known_hosts file fails fast at construction (before any
// handshake), so a corrupt file surfaces immediately rather than
// silently degrading verification. A valid host + key type with a
// corrupt base64 key blob is rejected by knownhosts.New's parser.
func TestKnownHostsCallback_MalformedFileFailsFast(t *testing.T) {
	khPath := newKHPath(t)

	// Valid leading fields so the parser reaches base64 decoding, then a
	// blob that is not valid base64 ('@' and '!' are outside the alphabet).
	garbage := "corrupt.example.com ssh-ed25519 this_is_not_valid_base64!!!@@@\n"
	if err := os.WriteFile(khPath, []byte(garbage), 0o600); err != nil {
		t.Fatalf("write malformed known_hosts: %v", err)
	}

	if _, err := knownHostsCallback(khPath, discardLogger()); err == nil {
		t.Fatal("a malformed known_hosts file should fail callback construction, got nil error")
	}
}

// serializeKey returns the base64 wire encoding of a public key as it
// appears in a known_hosts line, for substring assertions.
func serializeKey(key ssh.PublicKey) string {
	fields := strings.Fields(string(ssh.MarshalAuthorizedKey(key)))
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// countHostLines counts non-comment, non-blank known_hosts lines whose
// host field references the given host substring.
func countHostLines(contents, host string) int {
	n := 0
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, host) {
			n++
		}
	}
	return n
}
