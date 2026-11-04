// Startup without network: the first SSH connection of a server that starts
// at login (a service manager, `mole up -d` in a login script) is often
// attempted before Wi-Fi or a VPN is up. Transient failures are retried with
// capped exponential backoff instead of ending the run; failures a retry
// cannot fix (credentials, host keys, configuration) still fail at once.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"
)

// connectRetryMessage is logged when the first attempt fails in a way
// worth retrying; daemonize looks for it in the log to tell the user.
const connectRetryMessage = "remote not reachable yet; retrying"

// backoff is a capped exponential retry schedule.
type backoff struct {
	initial, max time.Duration
}

// startupBackoff paces retries of the first connection: 1s, 2s, 4s, ... 30s.
var startupBackoff = backoff{initial: time.Second, max: 30 * time.Second}

// connectWhenReachable calls dial until it succeeds, fails permanently (see
// isTransientConnectError) or ctx is done, in which case it returns
// ctx.Err() promptly. The first transient failure is logged at warn; later
// ones at debug, or info when the error changes and every tenth attempt.
func connectWhenReachable[T io.Closer](ctx context.Context, log *slog.Logger, b backoff, dial func() (T, error)) (T, error) {
	var zero T
	delay := b.initial
	lastErr := ""
	for attempt := 1; ; attempt++ {
		v, err := dialCancellable(ctx, dial)
		if err == nil {
			if attempt > 1 {
				log.Info("connected to remote", "attempts", attempt)
			}
			return v, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if !isTransientConnectError(err) {
			return zero, err
		}
		switch msg := err.Error(); {
		case attempt == 1:
			log.Warn(connectRetryMessage, "err", err, "retry_in", delay)
		case msg != lastErr || attempt%10 == 0:
			log.Info("still waiting for the remote", "err", err, "attempt", attempt, "retry_in", delay)
		default:
			log.Debug("still waiting for the remote", "err", err, "attempt", attempt, "retry_in", delay)
		}
		lastErr = err.Error()

		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return zero, ctx.Err()
		case <-t.C:
		}
		delay = min(delay*2, b.max)
	}
}

// dialCancellable runs dial, which cannot be interrupted itself (each
// tunnel.New attempt is bounded by its own dial and handshake timeouts),
// and returns as soon as ctx is done. A connection that completes after
// that is closed.
func dialCancellable[T io.Closer](ctx context.Context, dial func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := dial()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		go func() {
			if r := <-done; r.err == nil {
				_ = r.v.Close()
			}
		}()
		var zero T
		return zero, ctx.Err()
	}
}

// permanentConnectMarkers identify failures a retry cannot fix, in error
// text that carries no typed cause: authentication, host-key verification
// and known_hosts problems, unusable algorithms.
var permanentConnectMarkers = []string{
	"unable to authenticate",
	"no supported methods remain",
	"no ssh auth methods",
	"too many authentication failures",
	"host key",
	"knownhosts",
	"known_hosts",
	"no common algorithm",
}

// transientConnectMarkers identify network failures in error text that
// lost its typed cause (e.g. formatted with %v by a wrapper).
var transientConnectMarkers = []string{
	"connection refused",
	"connection reset",
	"connection aborted",
	"connection timed out",
	"no route to host",
	"network is unreachable",
	"network is down",
	"host is down",
	"i/o timeout",
	"no such host",
	"temporary failure in name resolution",
	"server misbehaving",
	"assign requested address",
	"broken pipe",
	"handshake failed: eof",
	"unexpected eof",
}

// transientErrnos are socket errors seen while the network comes up.
var transientErrnos = []error{
	syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED,
	syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ENETDOWN,
	syscall.EHOSTDOWN, syscall.ETIMEDOUT, syscall.EPIPE, syscall.EADDRNOTAVAIL,
}

// isTransientConnectError reports whether a failed connection attempt is
// worth retrying: the network or DNS not ready, the remote not answering
// yet, a handshake cut short. Authentication, host-key and configuration
// failures, cancellation and anything unrecognised are permanent.
func isTransientConnectError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	// Host-key verdicts are final, whatever else the chain carries.
	var keyErr *knownhosts.KeyError
	var revokedErr *knownhosts.RevokedError
	if errors.As(err, &keyErr) || errors.As(err, &revokedErr) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range permanentConnectMarkers {
		if strings.Contains(msg, m) {
			return false
		}
	}

	var dnsErr *net.DNSError
	var opErr *net.OpError
	if errors.As(err, &dnsErr) || errors.As(err, &opErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for _, errno := range transientErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	for _, m := range transientConnectMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
