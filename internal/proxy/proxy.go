// Package proxy implements a single TCP forwarder: it accepts
// connections on a local listener and bridges them to a remote address
// via a Dialer (typically the SSH tunnel manager).
package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net"
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
}

// Serve binds to ln and, for each accepted connection, dials the
// remote address and bridges bytes bidirectionally until either side
// closes. Returns nil when ln is closed.
func Serve(ln net.Listener, dial Dialer, remoteAddr string, hooks Hooks, log *slog.Logger) error {
	for {
		local, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Warn("accept failed", "err", err)
			continue
		}
		go handle(local, dial, remoteAddr, hooks, log)
	}
}

func handle(local net.Conn, dial Dialer, remoteAddr string, hooks Hooks, log *slog.Logger) {
	defer local.Close()
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
		_, _ = io.Copy(remote, local)
		closeWrite(remote)
		errc <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, remote)
		closeWrite(local)
		errc <- struct{}{}
	}()
	<-errc
	<-errc
}

// closeWrite best-effort signals "no more data from this side" to the
// peer. Silently no-ops for non-TCP conns (e.g. in-memory fakes in
// tests).
func closeWrite(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}
