// Package tunnel manages a single SSH client connection to the remote
// host and dials TCP connections through it. It transparently
// reconnects on failure so the proxy layer above doesn't need to
// worry about transient network issues.
package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Manager owns the SSH client and transparently reconnects on failure.
type Manager struct {
	addr   string // host:port to dial
	config *ssh.ClientConfig
	log    *slog.Logger

	mu     sync.RWMutex
	client *ssh.Client
}

// New constructs a Manager and opens the first SSH connection using the
// resolved remote (see ResolveRemote).
func New(r Remote, log *slog.Logger) (*Manager, error) {
	cfg, err := buildSSHConfig(r.User, r.IdentityFiles)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		addr:   r.Addr,
		config: cfg,
		log:    log,
	}

	if err := m.connect(); err != nil {
		return nil, err
	}
	return m, nil
}

// Addr returns the SSH target (host:port) this manager is connected to.
func (m *Manager) Addr() string {
	return m.addr
}

// connect establishes a new SSH client and stores it. The previous
// client (if any) is closed.
func (m *Manager) connect() error {
	client, err := ssh.Dial("tcp", m.addr, m.config)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", m.addr, err)
	}
	m.mu.Lock()
	if m.client != nil {
		_ = m.client.Close()
	}
	m.client = client
	m.mu.Unlock()
	return nil
}

// Dial opens a TCP connection through the SSH tunnel to addr (typically
// "127.0.0.1:PORT"). If the SSH client is dead, Dial attempts to
// reconnect once before failing.
func (m *Manager) Dial(network, addr string) (net.Conn, error) {
	m.mu.RLock()
	client := m.client
	m.mu.RUnlock()

	if client == nil {
		if err := m.connect(); err != nil {
			return nil, err
		}
		m.mu.RLock()
		client = m.client
		m.mu.RUnlock()
	}

	conn, err := client.Dial(network, addr)
	if err == nil {
		return conn, nil
	}

	// A rejected channel open (e.g. "connect failed (Connection
	// refused)") means the remote refused the forwarded connection —
	// nothing is listening on that port, or an ACL blocked it. The SSH
	// transport itself is fine, so surface the error without tearing
	// down and reconnecting. This is the common case during auto-
	// discovery, where most probed ports are closed.
	var openErr *ssh.OpenChannelError
	if errors.As(err, &openErr) {
		return nil, err
	}

	// Otherwise the transport itself likely died — try once to reconnect.
	m.log.Warn("ssh dial failed, attempting reconnect", "err", err, "addr", addr)
	m.mu.Lock()
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
	m.mu.Unlock()

	if rerr := m.connect(); rerr != nil {
		return nil, fmt.Errorf("reconnect: %w (original: %v)", rerr, err)
	}

	m.mu.RLock()
	client = m.client
	m.mu.RUnlock()
	return client.Dial(network, addr)
}

// Watch periodically verifies the SSH connection and reconnects if it
// has died. Runs until ctx is cancelled.
func (m *Manager) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.mu.RLock()
			c := m.client
			m.mu.RUnlock()
			if c == nil {
				if err := m.connect(); err != nil {
					m.log.Debug("reconnect failed", "err", err)
				} else {
					m.log.Info("reconnected to remote")
				}
				continue
			}
			// Probe by opening a session and immediately closing it.
			sess, err := c.NewSession()
			if err != nil {
				m.log.Warn("ssh session failed, reconnecting", "err", err)
				_ = c.Close()
				m.mu.Lock()
				m.client = nil
				m.mu.Unlock()
				if rerr := m.connect(); rerr != nil {
					m.log.Debug("reconnect failed", "err", rerr)
				} else {
					m.log.Info("reconnected to remote")
				}
				continue
			}
			_ = sess.Close()
		}
	}
}

// Close shuts down the SSH client.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil {
		return m.client.Close()
	}
	return nil
}

func buildSSHConfig(user string, identityFiles []string) (*ssh.ClientConfig, error) {
	methods, err := authMethods(identityFiles)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User: user,
		Auth: methods,
		// TODO: implement known_hosts verification in v0.2.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
}

func authMethods(identityFiles []string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	// 0. Explicit identity files from ssh_config (resolved via
	//    `ssh -G` when the remote is a Host alias). These reflect the
	//    user's intent for this host, so try them first.
	for _, path := range identityFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	// 1. ssh-agent (preferred).
	//
	// dialAgent is OS-specific (see agent_unix.go / agent_windows.go):
	// it dials a Unix socket on Linux/macOS and a Windows named pipe
	// on Windows. It fails silently (returns an error) when no agent
	// is configured, which lets us fall through to direct key files.
	if conn, err := dialAgent(os.Getenv("SSH_AUTH_SOCK")); err == nil {
		if signers, err := agent.NewClient(conn).Signers(); err == nil && len(signers) > 0 {
			methods = append(methods, ssh.PublicKeys(signers...))
			// conn stays open for the lifetime of the SSH session —
			// the ssh.PublicKeys wrapper above retains a reference
			// and uses it for signing operations.
		}
	}

	// 2. Default key files under ~/.ssh.
	home, err := os.UserHomeDir()
	if err == nil {
		for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa", "id_dsa"} {
			path := filepath.Join(home, ".ssh", name)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			signer, err := ssh.ParsePrivateKey(data)
			if err != nil {
				continue
			}
			methods = append(methods, ssh.PublicKeys(signer))
		}
	}

	if len(methods) == 0 {
		return nil, errors.New("no SSH auth methods: set up ssh-agent or ~/.ssh/id_*")
	}
	return methods, nil
}

// Remote holds resolved SSH connection parameters.
type Remote struct {
	User          string   // login user
	Addr          string   // host:port to dial
	IdentityFiles []string // key files from ssh_config, may be empty
}

// ResolveRemote turns a remote spec into concrete connection params.
//
//   - A spec containing '@' is parsed directly as user@host[:port].
//   - Otherwise it's treated as an ssh_config Host alias and resolved
//     with `ssh -G <alias>`, so ~/.ssh/config directives (HostName,
//     User, Port, IdentityFile, Include, Match) are honoured. This is
//     why a bare alias like "dev" connects to the real host instead of
//     trying to dial a literal "dev".
func ResolveRemote(spec string, defaultPort int) (Remote, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Remote{}, errors.New("remote is required")
	}
	if strings.Contains(spec, "@") {
		user, addr, err := ParseRemote(spec, defaultPort)
		if err != nil {
			return Remote{}, err
		}
		return Remote{User: user, Addr: addr}, nil
	}
	return resolveAlias(spec, defaultPort)
}

// resolveAlias shells out to `ssh -G <alias>` and parses the fully
// resolved configuration. Relying on OpenSSH means every ssh_config
// feature (Include, Match, canonicalisation) works without us
// re-implementing the parser.
func resolveAlias(alias string, defaultPort int) (Remote, error) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return Remote{}, fmt.Errorf("remote %q looks like an ssh host alias but 'ssh' is not on PATH to resolve it; use user@host[:port] instead", alias)
	}
	out, err := exec.Command(sshBin, "-G", alias).Output()
	if err != nil {
		return Remote{}, fmt.Errorf("resolve ssh alias %q via 'ssh -G': %w", alias, err)
	}

	var host, user, port string
	var ids []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "hostname":
			host = fields[1]
		case "user":
			user = fields[1]
		case "port":
			port = fields[1]
		case "identityfile":
			ids = append(ids, expandHome(strings.Join(fields[1:], " ")))
		}
	}
	if host == "" {
		host = alias // ssh always prints hostname, but be defensive
	}
	if port == "" {
		port = strconv.Itoa(defaultPort)
	}
	return Remote{
		User:          user,
		Addr:          net.JoinHostPort(host, port),
		IdentityFiles: ids,
	}, nil
}

// expandHome expands a leading "~/" (or bare "~") to the user's home
// directory. `ssh -G` prints identityfile paths with "~" unexpanded.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// ParseRemote splits "user@host[:port]" into (user, host:port). If the
// port is missing, defaultPort is appended.
func ParseRemote(remote string, defaultPort int) (user, addr string, err error) {
	at := strings.LastIndex(remote, "@")
	if at < 0 {
		return "", "", errors.New("remote must be in the form user@host[:port]")
	}
	user = remote[:at]
	host := remote[at+1:]
	if user == "" || host == "" {
		return "", "", errors.New("remote must be in the form user@host[:port]")
	}
	if !strings.Contains(host, ":") {
		host = net.JoinHostPort(host, strconv.Itoa(defaultPort))
	}
	return user, host, nil
}
