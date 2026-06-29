package proxy

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// recordingDialer proxies every Dial to a fixed address. Used when the
// test doesn't care about the remote side at all and just wants to
// observe what gets dialed.
type recordingDialer struct {
	mu       sync.Mutex
	calls    []string
	respErr  error
	respConn net.Conn // optional override
}

func (d *recordingDialer) Dial(network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.calls = append(d.calls, addr)
	d.mu.Unlock()
	if d.respConn != nil {
		return d.respConn, nil
	}
	return nil, d.respErr
}

func (d *recordingDialer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// echoServer accepts connections and echoes bytes back to the client
// (HTTP/1.0-style: when the client half-closes, the server echoes
// everything it has read and then half-closes its own write side).
// Returns when ln is closed.
func echoServer(t *testing.T) (net.Listener, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Echo everything the client sent until EOF.
				_, _ = io.Copy(c, c)
				// Tell the client we are done writing. The read
				// side stays open until the client closes it (or
				// the proxy tears down the connection).
				if tcp, ok := c.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
			}(c)
		}
	}()
	return ln, done
}

// dialerFunc adapts a function to the Dialer interface.
type dialerFunc func(network, addr string) (net.Conn, error)

func (f dialerFunc) Dial(network, addr string) (net.Conn, error) {
	return f(network, addr)
}

func TestServe_ClosedListenerReturnsNil(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &recordingDialer{}

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ln, d, "127.0.0.1:1234", Hooks{}, quietLogger()) }()

	// Give Serve a moment to reach Accept; closing too early would
	// race and isn't a realistic shutdown anyway.
	time.Sleep(20 * time.Millisecond)

	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve(closed) = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after listener closed")
	}
}

func TestServe_ForwardsBytesBidirectionally(t *testing.T) {
	// Upstream: a real loopback echo server.
	echo, echoDone := echoServer(t)
	defer func() {
		_ = echo.Close()
		<-echoDone
	}()

	// Proxy: binds a local listener, dials the echo server through
	// our dialer.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}

	d := dialerFunc(func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", echo.Addr().String())
	})

	hooks := Hooks{}
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(proxyLn, d, echo.Addr().String(), hooks, quietLogger()) }()

	// Client side.
	client, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()

	msg := []byte("hello proxy")
	if _, err := client.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf, msg) {
		t.Errorf("echoed = %q, want %q", buf, msg)
	}

	_ = client.Close()
	_ = proxyLn.Close()
	<-errCh
}

func TestServe_DialFailureCallsOnDialFail(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxyLn.Close()

	var dialFails, connects, disconnects atomic.Int64
	hooks := Hooks{
		OnConnect:    func() { connects.Add(1) },
		OnDisconnect: func() { disconnects.Add(1) },
		OnDialFail:   func() { dialFails.Add(1) },
	}

	badErr := errors.New("simulated dial failure")
	d := &recordingDialer{respErr: badErr}

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(proxyLn, d, "127.0.0.1:9", hooks, quietLogger()) }()

	conn, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}

	// Read until EOF to confirm the proxy closed our side after the
	// dial failure.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.Copy(io.Discard, conn)
	if err == nil {
		// Some kernels return a clean EOF; that's fine.
	}
	_ = conn.Close()

	// Allow the goroutine to settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dialFails.Load() == 1 && connects.Load() == 1 && disconnects.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := d.callCount(); got != 1 {
		t.Errorf("dial calls = %d, want 1", got)
	}
	if got := dialFails.Load(); got != 1 {
		t.Errorf("OnDialFail calls = %d, want 1", got)
	}
	if got := connects.Load(); got != 1 {
		t.Errorf("OnConnect calls = %d, want 1", got)
	}
	if got := disconnects.Load(); got != 1 {
		t.Errorf("OnDisconnect calls = %d, want 1", got)
	}

	_ = proxyLn.Close()
	<-errCh
}

func TestServe_HooksAlwaysCalledOnce(t *testing.T) {
	// Even when the remote side hangs up immediately, OnConnect +
	// OnDisconnect should still fire.
	echo, echoDone := echoServer(t)
	defer func() {
		_ = echo.Close()
		<-echoDone
	}()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxyLn.Close()

	var connects, disconnects atomic.Int64
	hooks := Hooks{
		OnConnect:    func() { connects.Add(1) },
		OnDisconnect: func() { disconnects.Add(1) },
	}

	d := dialerFunc(func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", echo.Addr().String())
	})
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(proxyLn, d, echo.Addr().String(), hooks, quietLogger()) }()

	conn, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close() // immediate close

	// Wait for hooks.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if connects.Load() == 1 && disconnects.Load() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if connects.Load() != 1 {
		t.Errorf("OnConnect = %d, want 1", connects.Load())
	}
	if disconnects.Load() != 1 {
		t.Errorf("OnDisconnect = %d, want 1", disconnects.Load())
	}
}

func TestServe_ConcurrentConnections(t *testing.T) {
	echo, echoDone := echoServer(t)
	defer func() {
		_ = echo.Close()
		<-echoDone
	}()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxyLn.Close()

	var activePeak atomic.Int64
	var activeCur atomic.Int64
	var total atomic.Int64

	hooks := Hooks{
		OnConnect: func() {
			cur := activeCur.Add(1)
			// Track high-water mark.
			for {
				peak := activePeak.Load()
				if cur <= peak || activePeak.CompareAndSwap(peak, cur) {
					break
				}
			}
			total.Add(1)
		},
		OnDisconnect: func() {
			activeCur.Add(-1)
		},
	}

	d := dialerFunc(func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", echo.Addr().String())
	})
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(proxyLn, d, echo.Addr().String(), hooks, quietLogger()) }()

	const N = 25
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", proxyLn.Addr().String())
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			defer c.Close()
			_, _ = c.Write([]byte("ping"))
			buf := make([]byte, 4)
			_, _ = io.ReadFull(c, buf)
		}()
	}
	wg.Wait()

	// Give the proxy a moment to call OnDisconnect for every conn.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if activeCur.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := total.Load(); got != N {
		t.Errorf("total connections = %d, want %d", got, N)
	}
	if got := activeCur.Load(); got != 0 {
		t.Errorf("active connections after all done = %d, want 0", got)
	}
}

func TestServe_LargePayload(t *testing.T) {
	echo, echoDone := echoServer(t)
	defer func() {
		_ = echo.Close()
		<-echoDone
	}()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxyLn.Close()

	d := dialerFunc(func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", echo.Addr().String())
	})
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(proxyLn, d, echo.Addr().String(), Hooks{}, quietLogger()) }()

	c, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// 4 KiB — small enough to fit in a single TCP send buffer so the
	// full round-trip completes before the client half-closes.
	payload := bytes.Repeat([]byte("ABCD"), 1024)
	go func() {
		defer func() {
			if tcp, ok := c.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			} else {
				_ = c.Close()
			}
		}()
		_, _ = c.Write(payload)
	}()

	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("echoed payload length = %d, want %d", len(got), len(payload))
	}
}
