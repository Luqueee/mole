// Host key verification for the SSH transport.
//
// Replaces the old ssh.InsecureIgnoreHostKey() — which accepted ANY
// host key and left the tunnel wide open to man-in-the-middle — with
// real verification against ~/.ssh/known_hosts, plus a trust-on-first-
// use (TOFU) policy so a brand-new host doesn't need manual seeding.
package tunnel

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostKeyCallback returns the ssh.HostKeyCallback used for every
// connection. With insecure=true it preserves the legacy "accept
// anything" behaviour (dev escape hatch). Otherwise it verifies against
// the user's ~/.ssh/known_hosts (see knownHostsCallback).
func hostKeyCallback(insecure bool, log *slog.Logger) (ssh.HostKeyCallback, error) {
	if insecure {
		log.Warn("host key verification DISABLED (--insecure); the tunnel is vulnerable to man-in-the-middle")
		return ssh.InsecureIgnoreHostKey(), nil
	}
	khPath, err := knownHostsPath()
	if err != nil {
		return nil, err
	}
	return knownHostsCallback(khPath, log)
}

// knownHostsCallback builds a verifying callback backed by the
// known_hosts file at khPath. Outcomes per handshake:
//
//   - host known and key matches      → accept
//   - host known but key MISMATCHES   → reject (this is the MITM signal)
//   - host unknown                    → trust on first use: record the
//     key in known_hosts and accept, logging a warning
//
// The mismatch rejection is the security-critical case: it is what an
// attacker hits when impersonating a host you've connected to before.
//
// The file is re-read on every handshake (not once at construction) so
// that keys recorded via TOFU earlier in this process — or edited
// externally — are seen. SSH handshakes happen once per connection, not
// per forwarded byte, so the re-read cost is negligible, and it avoids
// appending a duplicate line every time a first-use host reconnects.
func knownHostsCallback(khPath string, log *slog.Logger) (ssh.HostKeyCallback, error) {
	if err := ensureKnownHosts(khPath); err != nil {
		return nil, fmt.Errorf("prepare known_hosts %s: %w", khPath, err)
	}
	// Validate once up front so a malformed file fails fast at startup
	// rather than on the first handshake.
	if _, err := knownhosts.New(khPath); err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", khPath, err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		verify, err := knownhosts.New(khPath)
		if err != nil {
			return fmt.Errorf("reload known_hosts %s: %w", khPath, err)
		}

		verr := verify(hostname, remote, key)
		if verr == nil {
			return nil // known and matches
		}

		// A keyed KeyError (Want non-empty) means the host IS known but
		// the presented key matches none of the stored keys — exactly
		// the MITM scenario. Never silently accept it.
		var keyErr *knownhosts.KeyError
		if errors.As(verr, &keyErr) && len(keyErr.Want) > 0 {
			return fmt.Errorf(
				"host key mismatch for %s: the remote presented a key that does not match %s — "+
					"this may be an attack. If the host legitimately changed keys, remove its line(s) from %s and retry: %w",
				hostname, khPath, khPath, verr)
		}

		// Host is unknown. Trust on first use: persist the key and
		// accept. The daemon runs unattended, so an interactive prompt
		// isn't an option; recording it still gives mismatch protection
		// on every subsequent connection.
		if addErr := appendKnownHost(khPath, hostname, remote, key); addErr != nil {
			return fmt.Errorf("record new host key for %s: %w", hostname, addErr)
		}
		log.Warn("new host key trusted on first use",
			"host", hostname,
			"fingerprint", ssh.FingerprintSHA256(key),
			"known_hosts", khPath)
		return nil
	}, nil
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

// appendKnownHost appends a known_hosts line for the host key. Both the
// dialed hostname and the resolved remote address are recorded (when
// they differ), mirroring OpenSSH so a later connect by either matches.
func appendKnownHost(path, hostname string, remote net.Addr, key ssh.PublicKey) error {
	addrs := []string{knownhosts.Normalize(hostname)}
	if remote != nil {
		if ra := knownhosts.Normalize(remote.String()); ra != addrs[0] {
			addrs = append(addrs, ra)
		}
	}
	line := knownhosts.Line(addrs, key)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}
