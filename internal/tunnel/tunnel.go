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
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	NewSession() (*ssh.Session, error)
	Close() error
}

// Manager owns the SSH client and transparently reconnects on failure.
type Manager struct {
	addr   string // host:port to dial
	config *ssh.ClientConfig
	log    *slog.Logger
	dial   func() (sshConn, error) // opens a fresh transport; swappable in tests
	// dialContext opens a fresh transport whose network operations observe
	// ctx. Context-aware dials use a temporary transport so a cancelled
	// channel open never has to close the manager's live client.
	dialContext func(context.Context) (sshConn, error)

	// agentConn is retained by agent-backed signers in config and must stay
	// open until the manager is closed.
	agentConn net.Conn

	mu     sync.RWMutex
	client sshConn

	// reconnectMu serialises reconnects so a herd of concurrent dials
	// failing on a dead transport triggers ONE redial, not one per port.
	// A channel, rather than sync.Mutex, lets context-aware callers stop
	// waiting when their request is cancelled.
	reconnectMu   chan struct{}
	reconnectOnce sync.Once
}

// dialAgentFunc is swappable in tests so agent connection ownership can be
// checked without relying on a user's local SSH agent.
var dialAgentFunc = dialAgent

// New constructs a Manager and opens the first SSH connection using the
// resolved remote (see ResolveRemote).
func New(r Remote, insecure bool, log *slog.Logger) (*Manager, error) {
	cfg, agentConn, err := buildSSHConfig(r.User, r.IdentityFiles, r.Addr, insecure, log)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		addr:      r.Addr,
		config:    cfg,
		log:       log,
		agentConn: agentConn,
	}
	// Default transport opener: a real SSH dial. Tests override m.dial
	// before triggering reconnects.
	m.dial = func() (sshConn, error) {
		return dialRemote(r, cfg, insecure, log)
	}
	m.dialContext = func(ctx context.Context) (sshConn, error) {
		return dialRemoteContext(ctx, r, cfg, insecure, log)
	}

	if err := m.connect(); err != nil {
		if agentConn != nil {
			_ = agentConn.Close()
		}
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
	return m.installClient(client)
}

// connectContext establishes a new client using the context-aware transport
// opener. It does not replace the current client until the new transport is
// fully ready.
func (m *Manager) connectContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("tunnel: nil reconnect context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.dialContext == nil {
		return errors.New("tunnel: context-aware SSH dial is not configured")
	}
	client, err := m.dialContext(ctx)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", m.addr, err)
	}
	if err := ctx.Err(); err != nil {
		_ = client.Close()
		return err
	}
	return m.installClient(client)
}

func (m *Manager) installClient(client sshConn) error {
	if client == nil {
		return errors.New("tunnel: SSH dial returned a nil client")
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
	return m.reconnectContext(context.Background(), dead)
}

// reconnectContext is the context-aware form of reconnect. The lock
// acquisition and the replacement transport both observe ctx, so a caller
// cannot remain stuck behind another reconnect after its request expires.
func (m *Manager) reconnectContext(ctx context.Context, dead sshConn) (sshConn, error) {
	if ctx == nil {
		return nil, errors.New("tunnel: nil reconnect context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.reconnectOnce.Do(func() {
		m.reconnectMu = make(chan struct{}, 1)
		m.reconnectMu <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.reconnectMu:
	}
	defer func() { m.reconnectMu <- struct{}{} }()

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
	if err := m.connectContext(ctx); err != nil {
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

	// A rejected channel open means the remote refused the forwarded
	// connection. The SSH transport itself is still healthy.
	var openErr *ssh.OpenChannelError
	if errors.As(err, &openErr) {
		return nil, err
	}

	m.log.Debug("ssh dial failed, reconnecting", "err", err, "addr", addr)
	client, rerr := m.reconnect(client)
	if rerr != nil {
		return nil, fmt.Errorf("reconnect: %w (original: %v)", rerr, err)
	}
	return client.Dial(network, addr)
}

// DialContext opens a TCP connection through a temporary SSH transport while
// respecting ctx. The temporary transport is intentional: x/crypto/ssh
// cannot cancel one in-flight direct-tcpip channel without closing the whole
// client, which would tear down unrelated active forwards on m.client.
func (m *Manager) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if ctx == nil {
		return nil, errors.New("tunnel: nil dial context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.dialContext == nil {
		return nil, errors.New("tunnel: context-aware SSH dial is not configured")
	}
	client, err := m.dialContext(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := client.DialContext(ctx, network, addr)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		_ = client.Close()
		return nil, err
	}
	return &ownedConn{Conn: conn, owner: client}, nil
}

// ProbeDialer is a temporary, context-aware SSH transport used for one
// discovery sweep. It is separate from the manager's persistent client so a
// cancelled sweep cannot interrupt active forwards.
type ProbeDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	Close() error
}

// NewProbeDialer opens one temporary SSH transport for a discovery sweep.
// The caller owns it and must close it after all probe connections finish.
func (m *Manager) NewProbeDialer(ctx context.Context) (ProbeDialer, error) {
	if ctx == nil {
		return nil, errors.New("tunnel: nil probe dial context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.dialContext == nil {
		return nil, errors.New("tunnel: context-aware SSH dial is not configured")
	}
	client, err := m.dialContext(ctx)
	if err != nil {
		return nil, err
	}
	return &probeDialer{client: client}, nil
}

type probeDialer struct {
	client sshConn
	once   sync.Once
	err    error
}

func (d *probeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.client.DialContext(ctx, network, addr)
}

func (d *probeDialer) Close() error {
	d.once.Do(func() { d.err = d.client.Close() })
	return d.err
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
	client := m.client
	m.client = nil
	agentConn := m.agentConn
	m.agentConn = nil
	m.mu.Unlock()

	var closeErrs []error
	if client != nil {
		closeErrs = append(closeErrs, client.Close())
	}
	if agentConn != nil {
		closeErrs = append(closeErrs, agentConn.Close())
	}
	return errors.Join(closeErrs...)
}

func buildSSHConfig(user string, identityFiles []string, addr string, insecure bool, log *slog.Logger) (*ssh.ClientConfig, net.Conn, error) {
	methods, agentConn, err := authMethods(identityFiles)
	if err != nil {
		return nil, nil, err
	}
	keepAgentConn := false
	defer func() {
		if !keepAgentConn && agentConn != nil {
			_ = agentConn.Close()
		}
	}()
	hostKey, err := hostKeyCallback(insecure, log)
	if err != nil {
		return nil, nil, err
	}
	var hostKeyAlgorithms []string
	if !insecure {
		khPath, err := knownHostsPath()
		if err != nil {
			return nil, nil, err
		}
		hostKeyAlgorithms, err = knownHostKeyAlgorithms(khPath, addr, probeRemote(addr))
		if err != nil {
			return nil, nil, fmt.Errorf("select host-key algorithms for %s: %w", addr, err)
		}
	}
	config := &ssh.ClientConfig{
		User:              user,
		Auth:              methods,
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: hostKeyAlgorithms,
		Timeout:           10 * time.Second,
	}
	keepAgentConn = agentConn != nil
	return config, agentConn, nil
}

func authMethods(identityFiles []string) ([]ssh.AuthMethod, net.Conn, error) {
	var methods []ssh.AuthMethod
	var agentConn net.Conn

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
	if conn, err := dialAgentFunc(os.Getenv("SSH_AUTH_SOCK")); err == nil {
		if signers, err := agent.NewClient(conn).Signers(); err == nil && len(signers) > 0 {
			methods = append(methods, ssh.PublicKeys(signers...))
			agentConn = conn
			// conn stays open for the lifetime of the SSH session —
			// the ssh.PublicKeys wrapper above retains a reference
			// and uses it for signing operations.
		} else {
			_ = conn.Close()
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
		return nil, nil, errors.New("no SSH auth methods: set up ssh-agent or ~/.ssh/id_*")
	}
	return methods, agentConn, nil
}

// jumpSSHConn keeps every transport in a ProxyJump chain alive for the
// lifetime of the target connection.
type jumpSSHConn struct {
	client     *ssh.Client
	jumps      []*ssh.Client
	agentConns []net.Conn
}

func (c *jumpSSHConn) Dial(network, addr string) (net.Conn, error) {
	return c.client.Dial(network, addr)
}

func (c *jumpSSHConn) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return dialSSHChannelContext(ctx, c.client, network, addr)
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
	for i := len(c.agentConns) - 1; i >= 0; i-- {
		if closeErr := c.agentConns[i].Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

// contextSSHConn wraps a direct SSH client so context-aware channel opens
// can close the temporary client if the open is cancelled.
type contextSSHConn struct {
	client *ssh.Client
}

func (c *contextSSHConn) Dial(network, addr string) (net.Conn, error) {
	return c.client.Dial(network, addr)
}

func (c *contextSSHConn) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return dialSSHChannelContext(ctx, c.client, network, addr)
}

func (c *contextSSHConn) NewSession() (*ssh.Session, error) {
	return c.client.NewSession()
}

func (c *contextSSHConn) Close() error {
	return c.client.Close()
}

// ownedConn keeps the temporary SSH transport alive for the lifetime of the
// forwarded connection. Closing the returned connection closes both the SSH
// channel and its dedicated transport.
type ownedConn struct {
	net.Conn
	owner sshConn
	once  sync.Once
	err   error
}

func (c *ownedConn) Close() error {
	c.once.Do(func() {
		if err := c.Conn.Close(); err != nil {
			c.err = err
		}
		if err := c.owner.Close(); c.err == nil {
			c.err = err
		}
	})
	return c.err
}

// dialSSHChannelContext opens one direct-tcpip channel on a temporary SSH
// client. x/crypto/ssh's Client.DialContext returns as soon as ctx expires,
// but its internal Client.Dial goroutine can remain blocked waiting for the
// remote channel response. Closing this temporary client and waiting for that
// goroutine to finish avoids the leak without disrupting active forwards on
// the manager's persistent client.
func dialSSHChannelContext(ctx context.Context, client *ssh.Client, network, addr string) (net.Conn, error) {
	if ctx == nil {
		return nil, errors.New("tunnel: nil SSH channel context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := client.Dial(network, addr)
		done <- result{conn: conn, err: err}
	}()
	select {
	case res := <-done:
		return res.conn, res.err
	case <-ctx.Done():
		_ = client.Close()
		res := <-done
		if res.conn != nil {
			_ = res.conn.Close()
		}
		return nil, ctx.Err()
	}
}

// dialSSHClientContext establishes an SSH client with a context-aware TCP
// connect and a cancellable handshake. Closing the raw connection interrupts
// an in-flight handshake, and waiting for the result prevents a leaked
// handshake goroutine.
func dialSSHClientContext(ctx context.Context, network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	if ctx == nil {
		return nil, errors.New("tunnel: nil SSH dial context")
	}
	if config == nil {
		return nil, errors.New("tunnel: nil SSH client config")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := (&net.Dialer{Timeout: config.Timeout}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return newSSHClientContext(ctx, raw, addr, config)
}

func newSSHClientContext(ctx context.Context, raw net.Conn, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	if ctx == nil {
		_ = raw.Close()
		return nil, errors.New("tunnel: nil SSH handshake context")
	}
	if config == nil {
		_ = raw.Close()
		return nil, errors.New("tunnel: nil SSH client config")
	}
	handshakeCtx := ctx
	cancel := func() {}
	if config.Timeout > 0 {
		handshakeCtx, cancel = context.WithTimeout(ctx, config.Timeout)
	}
	defer cancel()
	type result struct {
		conn     ssh.Conn
		channels <-chan ssh.NewChannel
		requests <-chan *ssh.Request
		err      error
	}
	done := make(chan result, 1)
	go func() {
		conn, channels, requests, err := ssh.NewClientConn(raw, addr, config)
		done <- result{conn: conn, channels: channels, requests: requests, err: err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}
		return ssh.NewClient(res.conn, res.channels, res.requests), nil
	case <-handshakeCtx.Done():
		_ = raw.Close()
		res := <-done
		if res.conn != nil {
			_ = res.conn.Close()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, handshakeCtx.Err()
	}
}

// dialRemote connects directly or opens the configured ProxyJump chain before
// creating the SSH client for the target.
func dialRemote(remote Remote, config *ssh.ClientConfig, insecure bool, log *slog.Logger) (sshConn, error) {
	return dialRemoteContext(context.Background(), remote, config, insecure, log)
}

// dialRemoteContext is the context-aware counterpart of dialRemote. Every
// TCP connect, ProxyJump channel open, and SSH handshake can be interrupted
// by ctx. Partially-created jumps and their agent connections are closed on
// every failure path.
func dialRemoteContext(ctx context.Context, remote Remote, config *ssh.ClientConfig, insecure bool, log *slog.Logger) (sshConn, error) {
	if ctx == nil {
		return nil, errors.New("tunnel: nil remote dial context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(remote.ProxyJumps) == 0 {
		client, err := dialSSHClientContext(ctx, "tcp", remote.Addr, config)
		if err != nil {
			return nil, err
		}
		return &contextSSHConn{client: client}, nil
	}

	var jumps []*ssh.Client
	var agentConns []net.Conn
	keep := false
	defer func() {
		if !keep {
			closeSSHClients(jumps)
			closeSSHAgentConns(agentConns)
		}
	}()

	for _, jump := range remote.ProxyJumps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		jumpConfig, agentConn, err := buildSSHConfig(jump.User, jump.IdentityFiles, jump.Addr, insecure, log)
		if err != nil {
			return nil, err
		}
		if agentConn != nil {
			agentConns = append(agentConns, agentConn)
		}

		var client *ssh.Client
		if len(jumps) == 0 {
			client, err = dialSSHClientContext(ctx, "tcp", jump.Addr, jumpConfig)
		} else {
			conn, dialErr := dialSSHChannelContext(ctx, jumps[len(jumps)-1], "tcp", jump.Addr)
			if dialErr != nil {
				return nil, dialErr
			}
			client, err = newSSHClientContext(ctx, conn, jump.Addr, jumpConfig)
		}
		if err != nil {
			return nil, err
		}
		jumps = append(jumps, client)
	}

	conn, err := dialSSHChannelContext(ctx, jumps[len(jumps)-1], "tcp", remote.Addr)
	if err != nil {
		return nil, err
	}
	client, err := newSSHClientContext(ctx, conn, remote.Addr, config)
	if err != nil {
		return nil, err
	}
	keep = true
	return &jumpSSHConn{
		client:     client,
		jumps:      jumps,
		agentConns: agentConns,
	}, nil
}

func closeSSHClients(clients []*ssh.Client) {
	for i := len(clients) - 1; i >= 0; i-- {
		_ = clients[i].Close()
	}
}

func closeSSHAgentConns(conns []net.Conn) {
	for i := len(conns) - 1; i >= 0; i-- {
		_ = conns[i].Close()
	}
}

func closeSSHAgentConn(conn net.Conn) {
	if conn != nil {
		_ = conn.Close()
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
