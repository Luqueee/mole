// Package proxy implements a single TCP forwarder: it accepts
// connections on a local listener and bridges them to a remote address
// via a Dialer (typically the SSH tunnel manager).
package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"time"
)

// Dialer is anything that can produce a remote net.Conn given an
// address. The SSH tunnel manager satisfies this implicitly.
type Dialer interface {
	Dial(network, addr string) (net.Conn, error)
}

// Hooks lets the caller observe per-connection lifecycle events for
// stats/logging. All fields are optional.
type Hooks struct {
	OnConnect    func()
	OnDisconnect func()
	OnDialFail   func()
	// Dir is the direction this forwarder serves (DirPull when empty).
	Dir string
	// OnConn is called once per connection when it finishes, with the
	// timing and byte counts of the whole round trip. Port is left zero by
	// Serve; the caller that knows the port fills it in (see WithPort).
	OnConn func(ConnInfo)
}

// Directions of a forwarded connection. Pull: a local listener forwards to
// the remote (-L). Push: a remote listener forwards to a local service (-R).
const (
	DirPull = "pull"
	DirPush = "push"
)

// WithPort returns a copy whose OnConn reports connections as port.
func (h Hooks) WithPort(port int) Hooks {
	if next := h.OnConn; next != nil {
		h.OnConn = func(ci ConnInfo) {
			ci.Port = port
			next(ci)
		}
	}
	return h
}

// ConnInfo describes one finished forwarded connection.
type ConnInfo struct {
	Dir      string // DirPull or DirPush
	Port     int
	Start    time.Time
	Duration time.Duration
	BytesOut int64 // local -> remote
	BytesIn  int64 // remote -> local
	DialFail bool
}

// Serve binds to ln and, for each accepted connection, dials the
// remote address and bridges bytes bidirectionally until either side
// closes. Returns nil when ln is closed (net.ErrClosed, or io.EOF as
// returned by x/crypto/ssh remote listeners).
func Serve(ln net.Listener, dial Dialer, remoteAddr string, hooks Hooks, log *slog.Logger) error {
	const initialBackoff = 10 * time.Millisecond
	const maxBackoff = time.Second
	backoff := initialBackoff
	for {
		local, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
				return nil
			}
			log.Warn("accept failed", "err", err)
			time.Sleep(backoff)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = initialBackoff
		go handle(local, dial, remoteAddr, hooks, log)
	}
}

func handle(local net.Conn, dial Dialer, remoteAddr string, hooks Hooks, log *slog.Logger) {
	defer local.Close()
	info := ConnInfo{Start: time.Now(), Dir: hooks.Dir}
	if info.Dir == "" {
		info.Dir = DirPull
	}
	if hooks.OnConn != nil {
		defer func() {
			info.Duration = time.Since(info.Start)
			hooks.OnConn(info)
		}()
	}
	if hooks.OnConnect != nil {
		hooks.OnConnect()
	}
	defer func() {
		if hooks.OnDisconnect != nil {
			hooks.OnDisconnect()
		}
	}()

	remote, err := dial.Dial("tcp", remoteAddr)
	if err != nil {
		info.DialFail = true
		if hooks.OnDialFail != nil {
			hooks.OnDialFail()
		}
		log.Warn("dial remote failed", "addr", remoteAddr, "err", err)
		return
	}
	defer remote.Close()

	log.Debug("proxying", "local", local.RemoteAddr(), "remote", remoteAddr)

	// Bidirectional copy. Wait for BOTH copies to finish before
	// returning so we don't yank a connection out from under the
	// other direction and lose bytes still in flight.
	//
	// Propagate half-close to the peer so that, e.g., a server waiting
	// for QUIT after the client closed its write half actually observes
	// EOF and doesn't hang. We do the TCP closeWrite (best effort) once
	// each copy returns; the final Close below frees anything leftover.
	errc := make(chan struct{}, 2)
	go func() {
		info.BytesOut, _ = io.Copy(remote, local)
		closeWrite(remote)
		errc <- struct{}{}
	}()
	go func() {
		info.BytesIn, _ = io.Copy(local, remote)
		closeWrite(local)
		errc <- struct{}{}
	}()
	<-errc
	<-errc
}

// closeWrite best-effort signals "no more data from this side" to the
// peer. Covers *net.TCPConn and SSH channel conns (both have CloseWrite);
// no-ops for conns without it (e.g. in-memory fakes in tests).
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
