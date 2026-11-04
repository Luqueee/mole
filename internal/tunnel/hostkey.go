// Host key verification for the SSH transport.
//
// Replaces the old ssh.InsecureIgnoreHostKey() — which accepted ANY
// host key and left the tunnel wide open to man-in-the-middle — with
// real verification against the known_hosts files ssh_config selects
// (~/.ssh/known_hosts by default), plus a trust-on-first-use (TOFU)
// policy so a brand-new host doesn't need manual seeding.
package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostKeyCallback returns the ssh.HostKeyCallback for remote and the
// host-key algorithms to negotiate. With insecure=true it preserves the
// legacy "accept anything" behaviour (dev escape hatch). Otherwise it
// verifies against remote's known_hosts files (see knownHostsCallback).
func hostKeyCallback(remote Remote, insecure bool, log *slog.Logger) (ssh.HostKeyCallback, []string, error) {
	if insecure {
		log.Warn("host key verification DISABLED (--insecure); the tunnel is vulnerable to man-in-the-middle")
		return ssh.InsecureIgnoreHostKey(), nil, nil
	}
	policy, err := newHostKeyPolicy(remote)
	if err != nil {
		return nil, nil, err
	}
	callback, err := knownHostsCallback(policy, log)
	if err != nil {
		return nil, nil, err
	}
	algorithms, err := knownHostKeyAlgorithms(policy, remote.Addr, probeRemote(remote.Addr))
	if err != nil {
		return nil, nil, fmt.Errorf("select host-key algorithms for %s: %w", remote.Addr, err)
	}
	return callback, algorithms, nil
}

// hostKeyPolicy is how one remote's host key is verified and recorded.
type hostKeyPolicy struct {
	userFiles   []string // consulted; new keys are recorded in the first
	globalFiles []string // consulted, never written
	strict      bool     // reject unknown hosts instead of trusting them
	alias       string   // known_hosts name used instead of the host
	hash        bool     // record hashed host names
}

func newHostKeyPolicy(remote Remote) (hostKeyPolicy, error) {
	p := hostKeyPolicy{
		globalFiles: knownHostsFileList(remote.GlobalKnownHostsFiles),
		strict:      isYes(remote.StrictHostKeyChecking),
		alias:       remote.HostKeyAlias,
		hash:        remote.HashKnownHosts,
	}
	if remote.UserKnownHostsFiles == nil {
		khPath, err := knownHostsPath()
		if err != nil {
			return hostKeyPolicy{}, err
		}
		p.userFiles = []string{khPath}
	} else {
		p.userFiles = knownHostsFileList(remote.UserKnownHostsFiles)
	}
	return p, nil
}

// knownHostsFileList drops "none" entries and expands a leading "~".
func knownHostsFileList(files []string) []string {
	var out []string
	for _, f := range files {
		if f == "" || strings.EqualFold(f, "none") {
			continue
		}
		out = append(out, expandHome(f))
	}
	return out
}

// files returns the known_hosts files to consult, user files first: user
// files that exist (an unreadable one is an error, as before), and global
// files that can be read (OpenSSH skips the others).
func (p hostKeyPolicy) files() []string {
	var out []string
	for _, f := range p.userFiles {
		if _, err := os.Stat(f); err == nil {
			out = append(out, f)
		}
	}
	for _, f := range p.globalFiles {
		if r, err := os.Open(f); err == nil {
			_ = r.Close()
			out = append(out, f)
		}
	}
	return out
}

// lookupName is the address checked against known_hosts. HostKeyAlias
// replaces the host and, as in OpenSSH, carries no port.
func (p hostKeyPolicy) lookupName(hostname string) string {
	if p.alias != "" {
		return net.JoinHostPort(p.alias, "22")
	}
	return hostname
}

// verify checks key against the existing known_hosts files. With no files
// every host is unknown.
func (p hostKeyPolicy) verify(hostname string, remote net.Addr, key ssh.PublicKey) error {
	files := p.files()
	if len(files) == 0 {
		return &knownhosts.KeyError{}
	}
	check, err := knownhosts.New(files...)
	if err != nil {
		return fmt.Errorf("load known_hosts: %w", err)
	}
	return check(p.lookupName(hostname), remote, key)
}

// knownHostKeyAlgorithms returns the algorithms of keys already trusted for
// hostname. Restricting negotiation to these keys mirrors OpenSSH: a server
// that offers several host keys must not cause a mismatch merely because Go
// selects an unrecorded algorithm before the recorded one.
func knownHostKeyAlgorithms(p hostKeyPolicy, hostname string, remote net.Addr) ([]string, error) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate host-key probe: %w", err)
	}
	probe, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("create host-key probe: %w", err)
	}

	err = p.verify(hostname, remote, probe)
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) || len(keyErr.Want) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var algorithms []string
	for _, known := range keyErr.Want {
		for _, algorithm := range hostKeyAlgorithms(known.Key.Type()) {
			if _, ok := seen[algorithm]; ok {
				continue
			}
			seen[algorithm] = struct{}{}
			algorithms = append(algorithms, algorithm)
		}
	}
	return algorithms, nil
}

func hostKeyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

func probeRemote(addr string) net.Addr {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return &net.TCPAddr{}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		return &net.TCPAddr{}
	}
	return &net.TCPAddr{Port: portNumber}
}

// knownHostsCallback builds a verifying callback backed by the policy's
// known_hosts files. Outcomes per handshake:
//
//   - host known and key matches      → accept
//   - host known but key MISMATCHES   → reject (this is the MITM signal)
//   - key marked @revoked             → reject
//   - host unknown, strict checking   → reject with how to record it
//   - host unknown                    → trust on first use: record the
//     key in the first user known_hosts file and accept, logging a warning
//
// The mismatch rejection is the security-critical case: it is what an
// attacker hits when impersonating a host you've connected to before.
// It applies whatever StrictHostKeyChecking says.
//
// The files are re-read on every handshake (not once at construction) so
// that keys recorded via TOFU earlier in this process — or edited
// externally — are seen. SSH handshakes happen once per connection, not
// per forwarded byte, so the re-read cost is negligible, and it avoids
// appending a duplicate line every time a first-use host reconnects.
func knownHostsCallback(p hostKeyPolicy, log *slog.Logger) (ssh.HostKeyCallback, error) {
	if len(p.userFiles) > 0 {
		if err := ensureKnownHosts(p.userFiles[0]); err != nil {
			return nil, fmt.Errorf("prepare known_hosts %s: %w", p.userFiles[0], err)
		}
	}
	// Validate once up front so a malformed file fails fast at startup
	// rather than on the first handshake.
	if files := p.files(); len(files) > 0 {
		if _, err := knownhosts.New(files...); err != nil {
			return nil, fmt.Errorf("load known_hosts: %w", err)
		}
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		verr := p.verify(hostname, remote, key)
		if verr == nil {
			return nil // known and matches
		}
		name := knownhosts.Normalize(p.lookupName(hostname))

		var revoked *knownhosts.RevokedError
		if errors.As(verr, &revoked) {
			return fmt.Errorf("host key for %s is marked @revoked in %s:%d: %w",
				name, revoked.Revoked.Filename, revoked.Revoked.Line, verr)
		}

		// A keyed KeyError (Want non-empty) means the host IS known but
		// the presented key matches none of the stored keys — exactly
		// the MITM scenario. Never silently accept it.
		var keyErr *knownhosts.KeyError
		if errors.As(verr, &keyErr) && len(keyErr.Want) > 0 {
			known := keyErr.Want[0]
			return fmt.Errorf(
				"host key mismatch for %s: the remote presented a key that does not match the one recorded in %s:%d — "+
					"this may be an attack. If the host legitimately changed keys, remove its line(s) (ssh-keygen -R %s -f %s) and retry: %w",
				name, known.Filename, known.Line, name, known.Filename, verr)
		}

		fingerprint := ssh.FingerprintSHA256(key)
		if p.strict {
			return fmt.Errorf(
				"host key for %s (%s %s) is not in known_hosts and StrictHostKeyChecking is yes: "+
					"verify and record it by connecting once with ssh, or append the output of ssh-keyscan to %s after checking the fingerprint",
				name, key.Type(), fingerprint, p.recordTarget())
		}

		// Host is unknown. Trust on first use: persist the key and
		// accept. The daemon runs unattended, so an interactive prompt
		// isn't an option; recording it still gives mismatch protection
		// on every subsequent connection.
		if len(p.userFiles) == 0 {
			log.Warn("new host key accepted without recording it (UserKnownHostsFile none)",
				"host", name, "fingerprint", fingerprint)
			return nil
		}
		if addErr := appendKnownHost(p.userFiles[0], p.recordNames(hostname, remote), key, p.hash); addErr != nil {
			return fmt.Errorf("record new host key for %s: %w", name, addErr)
		}
		log.Warn("new host key trusted on first use",
			"host", name,
			"fingerprint", fingerprint,
			"known_hosts", p.userFiles[0])
		return nil
	}, nil
}

// recordTarget names the file TOFU would write, for error messages.
func (p hostKeyPolicy) recordTarget() string {
	if len(p.userFiles) == 0 {
		return "your known_hosts file"
	}
	return p.userFiles[0]
}

// recordNames are the known_hosts names for a new key: the dialed host and
// the resolved remote address (when they differ), mirroring OpenSSH so a
// later connect by either matches. With HostKeyAlias only the alias is
// recorded, as in OpenSSH.
func (p hostKeyPolicy) recordNames(hostname string, remote net.Addr) []string {
	names := []string{knownhosts.Normalize(p.lookupName(hostname))}
	if p.alias == "" && remote != nil {
		if ra := knownhosts.Normalize(remote.String()); ra != names[0] {
			names = append(names, ra)
		}
	}
	return names
}

// knownHostsPath returns the path to the user's known_hosts file.
func knownHostsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir for known_hosts: %w", err)
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// ensureKnownHosts makes sure the parent dir and an (at least empty)
// known_hosts file exist, so knownhosts.New doesn't fail on a fresh
// machine that has never run ssh.
func ensureKnownHosts(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// appendKnownHost appends known_hosts entries for key under the given
// (normalized) names. Hashed entries get one line per name, as OpenSSH
// writes them.
func appendKnownHost(path string, names []string, key ssh.PublicKey, hash bool) error {
	var lines []string
	if hash {
		for _, name := range names {
			lines = append(lines, knownhosts.Line([]string{knownhosts.HashHostname(name)}, key))
		}
	} else {
		lines = append(lines, knownhosts.Line(names, key))
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}
