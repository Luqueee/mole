package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"
)

// fakeConn stands in for *tunnel.Manager.
type fakeConn struct{ closed atomic.Bool }

func (c *fakeConn) Close() error { c.closed.Store(true); return nil }

func testLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

var fastBackoff = backoff{initial: time.Millisecond, max: 4 * time.Millisecond}

func refused() error {
	return fmt.Errorf("ssh dial 10.0.0.1:22: %w", &net.OpError{
		Op: "dial", Net: "tcp",
		Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
	})
}

func TestConnectWhenReachable_RetriesTransientFailures(t *testing.T) {
	log, buf := testLogger()
	want := &fakeConn{}
	calls := 0
	got, err := connectWhenReachable(context.Background(), log, fastBackoff, func() (*fakeConn, error) {
		calls++
		if calls < 4 {
			return nil, refused()
		}
		return want, nil
	})
	if err != nil || got != want {
		t.Fatalf("got %v, %v; want the connection", got, err)
	}
	if calls != 4 {
		t.Errorf("dial called %d times, want 4", calls)
	}
	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 1 || !strings.Contains(out, connectRetryMessage) {
		t.Errorf("want exactly one warning, the first failure; log:\n%s", out)
	}
	if n := strings.Count(out, "level=DEBUG"); n != 2 {
		t.Errorf("want repeated identical failures at debug (2), got %d; log:\n%s", n, out)
	}
	if !strings.Contains(out, "connected to remote") || !strings.Contains(out, "attempts=4") {
		t.Errorf("success after retries not logged; log:\n%s", out)
	}
}

func TestConnectWhenReachable_PermanentFailureFailsFast(t *testing.T) {
	log, buf := testLogger()
	authErr := errors.New("ssh dial host:22: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain")
	calls := 0
	_, err := connectWhenReachable(context.Background(), log, fastBackoff, func() (*fakeConn, error) {
		calls++
		return nil, authErr
	})
	if !errors.Is(err, authErr) || calls != 1 {
		t.Errorf("err = %v after %d calls; want the auth error after 1", err, calls)
	}
	if strings.Contains(buf.String(), connectRetryMessage) {
		t.Errorf("a permanent failure was announced as a retry:\n%s", buf.String())
	}
}

func TestConnectWhenReachable_StopsDuringBackoff(t *testing.T) {
	log, _ := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	_, err := connectWhenReachable(ctx, log, backoff{initial: time.Hour, max: time.Hour}, func() (*fakeConn, error) {
		time.AfterFunc(20*time.Millisecond, cancel)
		return nil, refused()
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %v; the backoff ignored the cancellation", elapsed)
	}
}

// tunnel.New cannot be interrupted: a stop request must not wait for it,
// and a connection that completes afterwards must be closed.
func TestConnectWhenReachable_StopsDuringDial(t *testing.T) {
	log, _ := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	late := &fakeConn{}
	errc := make(chan error, 1)
	go func() {
		_, err := connectWhenReachable(ctx, log, fastBackoff, func() (*fakeConn, error) {
			<-release
			return late, nil
		})
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not return while the dial was blocked")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for !late.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("connection completed after cancellation was not closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "op timed out" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestIsTransientConnectError(t *testing.T) {
	keyMismatch := &knownhosts.KeyError{Want: []knownhosts.KnownKey{{Filename: "/home/u/.ssh/known_hosts", Line: 3}}}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection refused", refused(), true},
		{"no route to host", &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.EHOSTUNREACH}}, true},
		{"network unreachable", fmt.Errorf("ssh dial: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}), true},
		{"dns not ready", fmt.Errorf("ssh dial: %w", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "dev.example", IsNotFound: true}}), true},
		{"dns error alone", &net.DNSError{Err: "server misbehaving", Name: "dev.example", IsTemporary: true}, true},
		{"handshake timeout", fmt.Errorf("ssh dial host:22: %w", context.DeadlineExceeded), true},
		{"handshake eof", fmt.Errorf("ssh dial host:22: %w", fmt.Errorf("ssh: handshake failed: %w", io.EOF)), true},
		{"handshake reset", fmt.Errorf("ssh: handshake failed: %w", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}), true},
		{"net.Error timeout", timeoutErr{}, true},
		{"reset as text only", errors.New("ssh dial host:22: read: connection reset by peer"), true},
		{"auth failure", errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"), false},
		{"no auth methods", errors.New("no SSH auth methods: set up ssh-agent or ~/.ssh/id_*"), false},
		{"host key mismatch", fmt.Errorf("ssh dial: %w", fmt.Errorf("ssh: handshake failed: %w",
			fmt.Errorf("host key mismatch for host:22: the remote presented a key that does not match: %w", keyMismatch))), false},
		{"host key mismatch as text", errors.New("ssh: handshake failed: host key mismatch for host:22"), false},
		{"unknown host, strict", fmt.Errorf("ssh: handshake failed: %w", &knownhosts.KeyError{}), false},
		{"revoked host key", &knownhosts.RevokedError{}, false},
		{"bad known_hosts", errors.New("load known_hosts /home/u/.ssh/known_hosts: knownhosts: missing host pattern"), false},
		{"cancelled", context.Canceled, false},
		{"unrecognised", errors.New("something else went wrong"), false},
	}
	for _, c := range cases {
		if got := isTransientConnectError(c.err); got != c.want {
			t.Errorf("%s: isTransientConnectError(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
