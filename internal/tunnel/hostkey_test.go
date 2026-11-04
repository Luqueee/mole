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

// testPolicy verifies against the single user known_hosts file khPath.
func testPolicy(khPath string) hostKeyPolicy {
	return hostKeyPolicy{userFiles: []string{khPath}}
}

// seedKnownHost records key for testHost in khPath as TOFU would.
func seedKnownHost(khPath string, key ssh.PublicKey) error {
	return appendKnownHost(khPath, testPolicy(khPath).recordNames(testHost, testRemote()), key, false)
}

// 1. Trust on first use: an unknown host is ACCEPTED and its key is
// PERSISTED to known_hosts, so a later mismatch can be detected.
func TestKnownHostsCallback_TOFUAcceptsAndPersistsUnknownHost(t *testing.T) {
	khPath := newKHPath(t)
	keyA := genKey(t)

	cb, err := knownHostsCallback(testPolicy(khPath), discardLogger())
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
	if err := seedKnownHost(khPath, keyA); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}

	cb, err := knownHostsCallback(testPolicy(khPath), discardLogger())
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

	if err := seedKnownHost(khPath, keyA); err != nil {
		t.Fatalf("seed known_hosts with trusted key: %v", err)
	}

	cb, err := knownHostsCallback(testPolicy(khPath), discardLogger())
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

	cb, err := knownHostsCallback(testPolicy(khPath), discardLogger())
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
	cb, _, err := hostKeyCallback(Remote{}, true, discardLogger())
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

	if _, err := knownHostsCallback(testPolicy(khPath), discardLogger()); err == nil {
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

func TestKnownHostKeyAlgorithms_PrefersRecordedKeyType(t *testing.T) {
	khPath := newKHPath(t)
	key := genKey(t)
	if err := seedKnownHost(khPath, key); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}

	algorithms, err := knownHostKeyAlgorithms(testPolicy(khPath), testHost, testRemote())
	if err != nil {
		t.Fatalf("knownHostKeyAlgorithms() error: %v", err)
	}
	if len(algorithms) != 1 || algorithms[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithms = %v, want [%s]", algorithms, ssh.KeyAlgoED25519)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestKnownHostsCallback_StrictHostKeyChecking(t *testing.T) {
	khPath := newKHPath(t)
	key := genKey(t)
	policy := testPolicy(khPath)
	policy.strict = true
	cb, err := knownHostsCallback(policy, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	err = cb(testHost, testRemote(), key)
	if err == nil {
		t.Fatal("StrictHostKeyChecking yes accepted an unknown host")
	}
	// cmd/mole's doctor classifies host-key failures by these words.
	for _, want := range []string{"StrictHostKeyChecking", "known_hosts", "ssh-keyscan", ssh.FingerprintSHA256(key)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if data := readFile(t, khPath); data != "" {
		t.Fatalf("strict checking recorded an unknown host:\n%s", data)
	}

	if err := seedKnownHost(khPath, key); err != nil {
		t.Fatal(err)
	}
	if err := cb(testHost, testRemote(), key); err != nil {
		t.Fatalf("strict checking rejected a known host: %v", err)
	}
}

func TestNewHostKeyPolicy_StrictValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for value, strict := range map[string]bool{
		"": false, "ask": false, "accept-new": false, "false": false, "no": false, "off": false,
		"true": true, "yes": true,
	} {
		p, err := newHostKeyPolicy(Remote{StrictHostKeyChecking: value})
		if err != nil {
			t.Fatal(err)
		}
		if p.strict != strict {
			t.Errorf("StrictHostKeyChecking %q: strict = %v, want %v", value, p.strict, strict)
		}
	}
}

// A changed key is refused whatever StrictHostKeyChecking says, including
// when the trusted key lives in a later or global known_hosts file.
func TestKnownHostsCallback_MismatchRejectedInEveryFile(t *testing.T) {
	dir := t.TempDir()
	user, user2, global := filepath.Join(dir, "known_hosts"), filepath.Join(dir, "known_hosts2"), filepath.Join(dir, "global")
	trusted := genKey(t)
	for _, file := range []string{user2, global} {
		t.Run(filepath.Base(file), func(t *testing.T) {
			for _, f := range []string{user, user2, global} {
				_ = os.Remove(f)
			}
			if err := appendKnownHost(file, []string{"workstation"}, trusted, false); err != nil {
				t.Fatal(err)
			}
			policy := hostKeyPolicy{userFiles: []string{user, user2}, globalFiles: []string{global}}
			cb, err := knownHostsCallback(policy, discardLogger())
			if err != nil {
				t.Fatal(err)
			}
			if err := cb(testHost, testRemote(), trusted); err != nil {
				t.Fatalf("key recorded in %s was not trusted: %v", file, err)
			}
			attacker := genKey(t)
			err = cb(testHost, testRemote(), attacker)
			if err == nil || !strings.Contains(err.Error(), "mismatch") || !strings.Contains(err.Error(), file) {
				t.Fatalf("mismatch against %s: err = %v, want a mismatch naming the file", file, err)
			}
			if strings.Contains(readFile(t, user), serializeKey(attacker)) {
				t.Fatal("SECURITY: the mismatched key was recorded")
			}
		})
	}
}

func TestKnownHostsCallback_TOFUWritesFirstUserFileOnly(t *testing.T) {
	dir := t.TempDir()
	user, user2, global := filepath.Join(dir, "known_hosts"), filepath.Join(dir, "known_hosts2"), filepath.Join(dir, "global")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	policy := hostKeyPolicy{userFiles: []string{user, user2}, globalFiles: []string{global, filepath.Join(dir, "absent")}}
	cb, err := knownHostsCallback(policy, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	key := genKey(t)
	if err := cb(testHost, testRemote(), key); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, user), serializeKey(key)) {
		t.Fatal("new key not recorded in the first user known_hosts file")
	}
	if readFile(t, user2) != "" || readFile(t, global) != "" {
		t.Fatal("new key recorded outside the first user known_hosts file")
	}
}

func TestKnownHostsCallback_UserKnownHostsFileNone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	policy, err := newHostKeyPolicy(Remote{UserKnownHostsFiles: []string{"none"}, GlobalKnownHostsFiles: []string{"none"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.userFiles) != 0 || len(policy.globalFiles) != 0 {
		t.Fatalf("policy = %#v, want no known_hosts files", policy)
	}
	cb, err := knownHostsCallback(policy, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := cb(testHost, testRemote(), genKey(t)); err != nil {
		t.Fatalf("unknown host rejected without StrictHostKeyChecking: %v", err)
	}
}

func TestNewHostKeyPolicy_ExpandsHomeAndDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	p, err := newHostKeyPolicy(Remote{})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".ssh", "known_hosts"); len(p.userFiles) != 1 || p.userFiles[0] != want || len(p.globalFiles) != 0 {
		t.Fatalf("default policy = %#v, want only %s", p, want)
	}
	p, err = newHostKeyPolicy(Remote{UserKnownHostsFiles: []string{"~/kh", "/abs/kh2"}, GlobalKnownHostsFiles: []string{"/etc/kh"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.userFiles[0] != filepath.Join(home, "kh") || p.userFiles[1] != "/abs/kh2" || p.globalFiles[0] != "/etc/kh" {
		t.Fatalf("policy = %#v", p)
	}
}

func TestKnownHostsCallback_HostKeyAlias(t *testing.T) {
	khPath := newKHPath(t)
	policy := testPolicy(khPath)
	policy.alias = "build-box"
	cb, err := knownHostsCallback(policy, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	key := genKey(t)
	if err := cb("10.0.0.7:2222", testRemote(), key); err != nil {
		t.Fatal(err)
	}
	data := readFile(t, khPath)
	if !strings.HasPrefix(data, "build-box ") || strings.Contains(data, "10.0.0.7") || strings.Contains(data, "127.0.0.1") {
		t.Fatalf("known_hosts = %q, want a line for the alias only", data)
	}
	// The alias identifies the host whatever address it is reached at.
	if err := cb("other-name:22", testRemote(), key); err != nil {
		t.Fatalf("alias lookup failed: %v", err)
	}
	if err := cb("other-name:22", testRemote(), genKey(t)); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("changed key under the alias: err = %v, want mismatch", err)
	}
	algorithms, err := knownHostKeyAlgorithms(policy, "elsewhere:22", testRemote())
	if err != nil || len(algorithms) != 1 || algorithms[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithms = %v, %v; want the alias's recorded key type", algorithms, err)
	}
}

func TestKnownHostsCallback_HashKnownHosts(t *testing.T) {
	khPath := newKHPath(t)
	policy := testPolicy(khPath)
	policy.hash = true
	cb, err := knownHostsCallback(policy, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	key := genKey(t)
	remote := testRemote()
	if err := cb(testHost, remote, key); err != nil {
		t.Fatal(err)
	}
	if err := cb(testHost, remote, key); err != nil {
		t.Fatalf("hashed entry did not match on reconnect: %v", err)
	}
	data := readFile(t, khPath)
	lines := strings.Split(strings.TrimSpace(data), "\n")
	// One hashed line per name (host and address), as OpenSSH writes them.
	if len(lines) != 2 || strings.Contains(data, "workstation") || strings.Contains(data, "127.0.0.1") {
		t.Fatalf("known_hosts = %q, want two hashed lines", data)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "|1|") {
			t.Fatalf("line %q is not hashed", line)
		}
	}
	if err := cb("127.0.0.1:22", remote, key); err != nil {
		t.Fatalf("hashed address entry did not match: %v", err)
	}
}

func TestKnownHostsCallback_RevokedKeyRejected(t *testing.T) {
	khPath := newKHPath(t)
	key := genKey(t)
	if err := os.WriteFile(khPath, []byte("@revoked * "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := knownHostsCallback(testPolicy(khPath), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := cb(testHost, testRemote(), key); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked key: err = %v, want rejection", err)
	}
	if countHostLines(readFile(t, khPath), "workstation") != 0 {
		t.Fatal("SECURITY: a revoked key was trusted on first use")
	}
}
