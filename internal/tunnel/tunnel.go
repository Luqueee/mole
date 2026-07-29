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

// sshConn is the subset of *ssh.Client the Manager depends on.
// *ssh.Client satisfies it as-is; tests inject a fake so reconnect
// coordination can be exercised without standing up a real SSH server.
type sshConn interface {
	Dial(network, addr string) (net.Conn, error)
	NewSession() (*ssh.Session, error)
	Close() error
}

// Manager owns the SSH client and transparently reconnects on failure.
type Manager struct {
	addr   string // host:port to dial
	config *ssh.ClientConfig
	log    *slog.Logger
	dial   func() (sshConn, error) // opens a fresh transport; swappable in tests

	mu     sync.RWMutex
	client sshConn

	// reconnectMu serialises reconnects so a herd of concurrent dials
	// failing on a dead transport triggers ONE redial, not one per port.
	reconnectMu sync.Mutex
}

// New constructs a Manager and opens the first SSH connection using the
// resolved remote (see ResolveRemote).
func New(r Remote, insecure bool, log *slog.Logger) (*Manager, error) {
	cfg, err := buildSSHConfig(r.User, r.IdentityFiles, r.Addr, insecure, log)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		addr:   r.Addr,
		config: cfg,
		log:    log,
	}
	// Default transport opener: a real SSH dial. Tests override m.dial
	// before triggering reconnects.
	m.dial = func() (sshConn, error) {
		return dialRemote(r, cfg, insecure, log)
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

// connect establishes a new client via m.dial and stores it, closing the
// previous one. Callers that need herd-safe reconnection should go
// through reconnect, not call connect directly.
func (m *Manager) connect() error {
	client, err := m.dial()
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

// reconnect replaces a dead transport, but only once across a herd of
// concurrent callers. dead is the client the caller observed as broken
// (nil if it just found no client). Holding reconnectMu, it re-checks the
// current client: if another goroutine already swapped in a fresh one,
// it returns that instead of dialing again — so N ports failing on a
// dropped transport produce ONE redial and ONE warn, not N.
func (m *Manager) reconnect(dead sshConn) (sshConn, error) {
	m.reconnectMu.Lock()
	defer m.reconnectMu.Unlock()

	m.mu.RLock()
	cur := m.client
	m.mu.RUnlock()
	if cur != nil && cur != dead {
		// Someone already reconnected while we waited for the lock.
		return cur, nil
	}

	if dead != nil {
		m.log.Warn("ssh transport down, reconnecting", "addr", m.addr)
	}
	if err := m.connect(); err != nil {
		return nil, err
	}
	m.log.Info("reconnected to remote")
	m.mu.RLock()
	cur = m.client
	m.mu.RUnlock()
	return cur, nil
}

// Dial opens a TCP connection through the SSH tunnel to addr (typically
// "127.0.0.1:PORT"). If the SSH client is dead, Dial attempts to
// reconnect once before failing.
func (m *Manager) Dial(network, addr string) (net.Conn, error) {
	m.mu.RLock()
	client := m.client
	m.mu.RUnlock()

	if client == nil {
		c, err := m.reconnect(nil)
		if err != nil {
			return nil, err
		}
		client = c
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

	// Otherwise the transport itself likely died. Route through reconnect
	// so a herd of ports failing at once shares ONE redial: the per-addr
	// detail is debug; reconnect emits the single transport-level warn.
	m.log.Debug("ssh dial failed, reconnecting", "err", err, "addr", addr)
	client, rerr := m.reconnect(client)
	if rerr != nil {
		return nil, fmt.Errorf("reconnect: %w (original: %v)", rerr, err)
	}
	return client.Dial(network, addr)
}

// Run executes a command on the remote over a fresh SSH session and
// returns its combined stdout+stderr. Used by listener-based discovery
// to enumerate what's actually listening on the remote.
func (m *Manager) Run(cmd string) ([]byte, error) {
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
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return sess.CombinedOutput(cmd)
}

// Watch periodically verifies the SSH connection and reconnects if it
// has died. Runs until ctx is cancelled. Reconnection goes through the
// shared singleflight path, so a Watch-detected drop and a Dial-detected
// drop can't double-redial.
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
				if _, err := m.reconnect(nil); err != nil {
					m.log.Debug("reconnect failed", "err", err)
				}
				continue
			}
			// Probe by opening a session and immediately closing it.
			sess, err := c.NewSession()
			if err != nil {
				m.log.Debug("ssh health check failed", "err", err)
				if _, rerr := m.reconnect(c); rerr != nil {
					m.log.Debug("reconnect failed", "err", rerr)
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

func buildSSHConfig(user string, identityFiles []string, addr string, insecure bool, log *slog.Logger) (*ssh.ClientConfig, error) {
	methods, err := authMethods(identityFiles)
	if err != nil {
		return nil, err
	}
	hostKey, err := hostKeyCallback(insecure, log)
	if err != nil {
		return nil, err
	}
	var hostKeyAlgorithms []string
	if !insecure {
		khPath, err := knownHostsPath()
		if err != nil {
			return nil, err
		}
		hostKeyAlgorithms, err = knownHostKeyAlgorithms(khPath, addr, probeRemote(addr))
		if err != nil {
			return nil, fmt.Errorf("select host-key algorithms for %s: %w", addr, err)
		}
	}
	return &ssh.ClientConfig{
		User:              user,
		Auth:              methods,
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: hostKeyAlgorithms,
		Timeout:           10 * time.Second,
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

// jumpSSHConn keeps every transport in a ProxyJump chain alive for the
// lifetime of the target connection.
type jumpSSHConn struct {
	client *ssh.Client
	jumps  []*ssh.Client
}

func (c *jumpSSHConn) Dial(network, addr string) (net.Conn, error) {
	return c.client.Dial(network, addr)
}

func (c *jumpSSHConn) NewSession() (*ssh.Session, error) {
	return c.client.NewSession()
}

func (c *jumpSSHConn) Close() error {
	err := c.client.Close()
	for i := len(c.jumps) - 1; i >= 0; i-- {
		if closeErr := c.jumps[i].Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

// dialRemote connects directly or opens the configured ProxyJump chain before
// creating the SSH client for the target.
func dialRemote(remote Remote, config *ssh.ClientConfig, insecure bool, log *slog.Logger) (sshConn, error) {
	if len(remote.ProxyJumps) == 0 {
		return ssh.Dial("tcp", remote.Addr, config)
	}

	var jumps []*ssh.Client
	for _, jump := range remote.ProxyJumps {
		jumpConfig, err := buildSSHConfig(jump.User, jump.IdentityFiles, jump.Addr, insecure, log)
		if err != nil {
			closeSSHClients(jumps)
			return nil, err
		}

		var client *ssh.Client
		if len(jumps) == 0 {
			client, err = ssh.Dial("tcp", jump.Addr, jumpConfig)
		} else {
			conn, dialErr := jumps[len(jumps)-1].Dial("tcp", jump.Addr)
			if dialErr != nil {
				closeSSHClients(jumps)
				return nil, dialErr
			}
			clientConn, channels, requests, handshakeErr := ssh.NewClientConn(conn, jump.Addr, jumpConfig)
			if handshakeErr != nil {
				closeSSHClients(jumps)
				return nil, handshakeErr
			}
			client = ssh.NewClient(clientConn, channels, requests)
		}
		if err != nil {
			closeSSHClients(jumps)
			return nil, err
		}
		jumps = append(jumps, client)
	}

	conn, err := jumps[len(jumps)-1].Dial("tcp", remote.Addr)
	if err != nil {
		closeSSHClients(jumps)
		return nil, err
	}
	clientConn, channels, requests, err := ssh.NewClientConn(conn, remote.Addr, config)
	if err != nil {
		closeSSHClients(jumps)
		return nil, err
	}
	return &jumpSSHConn{client: ssh.NewClient(clientConn, channels, requests), jumps: jumps}, nil
}

func closeSSHClients(clients []*ssh.Client) {
	for i := len(clients) - 1; i >= 0; i-- {
		_ = clients[i].Close()
	}
}

// Remote holds resolved SSH connection parameters.
type Remote struct {
	User          string   // login user
	Addr          string   // host:port to dial
	IdentityFiles []string // key files from ssh_config, may be empty
	ProxyJumps    []Remote // SSH hops, in connection order
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

	var host, user, port, proxyJump string
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
		case "proxyjump":
			proxyJump = strings.Join(fields[1:], " ")
		}
	}
	if host == "" {
		host = alias // ssh always prints hostname, but be defensive
	}
	if port == "" {
		port = strconv.Itoa(defaultPort)
	}
	proxyJumps, err := resolveProxyJumps(proxyJump, defaultPort)
	if err != nil {
		return Remote{}, fmt.Errorf("resolve ProxyJump for %q: %w", alias, err)
	}
	return Remote{
		User:          user,
		Addr:          net.JoinHostPort(host, port),
		IdentityFiles: ids,
		ProxyJumps:    proxyJumps,
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

func resolveProxyJumps(spec string, defaultPort int) ([]Remote, error) {
	if spec == "" || strings.EqualFold(spec, "none") {
		return nil, nil
	}

	var jumps []Remote
	for _, jumpSpec := range strings.Split(spec, ",") {
		jumpSpec = strings.TrimSpace(jumpSpec)
		if jumpSpec == "" {
			continue
		}
		jump, err := ResolveRemote(jumpSpec, defaultPort)
		if err != nil {
			return nil, err
		}
		jumps = append(jumps, jump.ProxyJumps...)
		jump.ProxyJumps = nil
		jumps = append(jumps, jump)
	}
	return jumps, nil
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
	if _, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		return user, host, nil
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return user, net.JoinHostPort(host, strconv.Itoa(defaultPort)), nil
}
