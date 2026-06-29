// Package tunnel manages a single SSH client connection to the remote
// host and dials TCP connections through it. It transparently
// reconnects on failure so the proxy layer above doesn't need to
// worry about transient network issues.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
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

// New constructs a Manager and opens the first SSH connection.
func New(addr, user string, log *slog.Logger) (*Manager, error) {
	cfg, err := buildSSHConfig(user)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		addr:   addr,
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

	// Connection failed — try once to reconnect.
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

func buildSSHConfig(user string) (*ssh.ClientConfig, error) {
	methods, err := authMethods()
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

func authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	// 1. ssh-agent (preferred).
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			signers, err := agent.NewClient(conn).Signers()
			if err == nil && len(signers) > 0 {
				methods = append(methods, ssh.PublicKeys(signers...))
			}
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
		host = fmt.Sprintf("%s:%d", host, defaultPort)
	}
	return user, host, nil
}