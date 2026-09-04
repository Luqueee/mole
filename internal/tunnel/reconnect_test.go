package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeNetConn is a minimal, stateless net.Conn handed back by liveConn.
// The reconnect tests only care that a non-nil connection comes back, so
// every method is a no-op; being stateless makes it safe to return the
// same shape from any number of concurrent Dial calls.
type fakeNetConn struct{}

func (fakeNetConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (fakeNetConn) Write(b []byte) (int, error)      { return len(b), nil }
func (fakeNetConn) Close() error                     { return nil }
func (fakeNetConn) LocalAddr() net.Addr              { return nil }
func (fakeNetConn) RemoteAddr() net.Addr             { return nil }
func (fakeNetConn) SetDeadline(time.Time) error      { return nil }
func (fakeNetConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeNetConn) SetWriteDeadline(time.Time) error { return nil }

// deadConn models a transport whose channel opens all fail with a plain
// (non-OpenChannelError) error — i.e. the SSH transport itself is gone,
// so Manager.Dial must route through reconnect.
//
// It doubles as a rendezvous barrier: Dial blocks every caller until
// `want` of them have arrived, then releases them together. This is what
// gives the collapse test its teeth. Without it, an instantaneous Dial
// lets the herd serialise — the first goroutine finishes reconnecting
// before the rest even read the client, so they never hit the failure
// path and even a naive per-goroutine reconnect would redial only once.
// Holding every caller at the dead transport until all N have failed
// guarantees a real concurrent herd: singleflight must collapse it to one
// redial, whereas a naive reconnect would fan out to N.
//
// closed is bumped atomically so the test can assert the dead transport
// was torn down exactly once.
type deadConn struct {
	want    int64         // number of callers to wait for (0 = no barrier)
	arrived int64         // callers that have reached Dial
	release chan struct{} // closed once `want` callers have arrived
	closed  int64         // times Close was called
}

func newDeadConn(want int64) *deadConn {
	return &deadConn{want: want, release: make(chan struct{})}
}

func (d *deadConn) Dial(network, addr string) (net.Conn, error) {
	if d.want > 0 {
		if atomic.AddInt64(&d.arrived, 1) == d.want {
			close(d.release) // last arrival frees the herd
		}
		<-d.release // park until everyone has failed together
	}
	return nil, io.EOF // a real transport error, NOT *ssh.OpenChannelError
}

func (d *deadConn) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.Dial(network, addr)
}

func (d *deadConn) NewSession() (*ssh.Session, error) {
	return nil, errors.New("dead transport")
}

func (d *deadConn) Close() error {
	atomic.AddInt64(&d.closed, 1)
	return nil
}

// liveConn models a healthy transport: every channel open succeeds. Dial
// is stateless and therefore safe to call concurrently any number of
// times. It is the fresh client a single redial installs.
type liveConn struct{}

func (l *liveConn) Dial(network, addr string) (net.Conn, error) {
	return fakeNetConn{}, nil
}

func (l *liveConn) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return l.Dial(network, addr)
}

func (l *liveConn) NewSession() (*ssh.Session, error) {
	return nil, errors.New("unused")
}

func (l *liveConn) Close() error { return nil }

type blockingContextConn struct {
	started chan struct{}
	once    sync.Once
}

func (c *blockingContextConn) Dial(network, addr string) (net.Conn, error) {
	return nil, errors.New("unexpected context-free dial")
}

func (c *blockingContextConn) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c.once.Do(func() { close(c.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *blockingContextConn) NewSession() (*ssh.Session, error) {
	return nil, errors.New("unused")
}

func (c *blockingContextConn) Close() error { return nil }

type trackedLiveConn struct {
	liveConn
	closed atomic.Int64
}

func (c *trackedLiveConn) Close() error {
	c.closed.Add(1)
	return nil
}

type blockingSSHTransport struct {
	opened    chan struct{}
	closed    chan struct{}
	openOnce  sync.Once
	closeOnce sync.Once
}

func (c *blockingSSHTransport) User() string          { return "test" }
func (c *blockingSSHTransport) SessionID() []byte     { return nil }
func (c *blockingSSHTransport) ClientVersion() []byte { return nil }
func (c *blockingSSHTransport) ServerVersion() []byte { return nil }
func (c *blockingSSHTransport) RemoteAddr() net.Addr  { return fakeAddr("remote") }
func (c *blockingSSHTransport) LocalAddr() net.Addr   { return fakeAddr("local") }
func (c *blockingSSHTransport) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return false, nil, errors.New("transport closed")
}
func (c *blockingSSHTransport) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	c.openOnce.Do(func() { close(c.opened) })
	<-c.closed
	return nil, nil, errors.New("transport closed")
}
func (c *blockingSSHTransport) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *blockingSSHTransport) Wait() error {
	<-c.closed
	return io.EOF
}

type fakeAddr string

func (a fakeAddr) Network() string { return "test" }
func (a fakeAddr) String() string  { return string(a) }

func TestManager_DialContextCancellation(t *testing.T) {
	client := &blockingContextConn{started: make(chan struct{})}
	persistent := &trackedLiveConn{}
	m := &Manager{
		addr:   "x:22",
		log:    discardLogger(),
		client: persistent,
		dialContext: func(context.Context) (sshConn, error) {
			return client, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := m.DialContext(ctx, "tcp", "127.0.0.1:3000")
		errCh <- err
	}()

	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("context-aware dial did not start")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DialContext() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DialContext() did not return after cancellation")
	}
	if got := persistent.closed.Load(); got != 0 {
		t.Fatalf("persistent SSH client closed %d times, want 0", got)
	}
}

func TestDialSSHChannelContextClosesTemporaryTransport(t *testing.T) {
	transport := &blockingSSHTransport{
		opened: make(chan struct{}),
		closed: make(chan struct{}),
	}
	incoming := make(chan ssh.NewChannel)
	requests := make(chan *ssh.Request)
	close(incoming)
	close(requests)
	client := ssh.NewClient(transport, incoming, requests)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := dialSSHChannelContext(ctx, client, "tcp", "127.0.0.1:3000")
		errCh <- err
	}()

	select {
	case <-transport.opened:
	case <-time.After(time.Second):
		t.Fatal("temporary SSH channel open did not start")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dialSSHChannelContext() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dialSSHChannelContext() did not return after cancellation")
	}
	select {
	case <-transport.closed:
	case <-time.After(time.Second):
		t.Fatal("temporary SSH transport remained open after cancellation")
	}
}

func TestNewSSHClientContextHonorsConfigTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	config := &ssh.ClientConfig{
		User:            "test",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Millisecond,
	}

	start := time.Now()
	_, err := newSSHClientContext(context.Background(), clientConn, "test", config)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("newSSHClientContext() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("SSH handshake timeout took %s, want it bounded", elapsed)
	}
}

func TestManagerReconnectContextCancellationWhileWaiting(t *testing.T) {
	started := make(chan struct{})
	firstDone := make(chan error, 1)
	m := &Manager{
		addr:   "x:22",
		log:    discardLogger(),
		client: &liveConn{},
		dialContext: func(ctx context.Context) (sshConn, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	dead := m.client
	firstCtx, firstCancel := context.WithCancel(context.Background())
	go func() {
		_, err := m.reconnectContext(firstCtx, dead)
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first reconnect did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := m.reconnectContext(ctx, dead)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting reconnect error = %v, want context deadline", err)
	}

	firstCancel()
	select {
	case reconnectErr := <-firstDone:
		if !errors.Is(reconnectErr, context.Canceled) {
			t.Fatalf("first reconnect error = %v, want context cancellation", reconnectErr)
		}
	case <-time.After(time.Second):
		t.Fatal("first reconnect did not release after cancellation")
	}
}

// TestManager_ReconnectCollapsesConcurrentDials is the teeth: N goroutines
// all fail their first dial on the SAME dead transport at the SAME instant
// (the deadConn barrier guarantees the overlap), then each calls
// Manager.Dial. Reconnection is singleflighted, so the first redial
// installs a fresh client and the rest reuse it — N simultaneous failures
// must collapse to exactly ONE redial and ONE teardown of the old
// transport. If reconnect regressed to per-goroutine redial, redials would
// climb to N and this assertion would fail.
func TestManager_ReconnectCollapsesConcurrentDials(t *testing.T) {
	const N = 24

	dead := newDeadConn(N)
	live := &liveConn{} // shared: every redial would hand back this same client

	var redials int64
	m := &Manager{
		addr: "x:22",
		log:  discardLogger(),
		dial: func() (sshConn, error) {
			atomic.AddInt64(&redials, 1)
			return live, nil
		},
		dialContext: func(context.Context) (sshConn, error) {
			atomic.AddInt64(&redials, 1)
			return live, nil
		},
	}
	m.client = dead

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, N)
	)
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // park until everyone is ready, then race together
			_, err := m.Dial("tcp", "127.0.0.1:3000")
			errs[idx] = err // distinct index per goroutine: race-free
		}(i)
	}
	close(start) // release the herd simultaneously
	wg.Wait()

	if got := atomic.LoadInt64(&redials); got != 1 {
		t.Fatalf("redials = %d, want 1 (concurrent dials must collapse to a single reconnect)", got)
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Dial returned error %v, want nil", i, err)
		}
	}

	if got := atomic.LoadInt64(&dead.closed); got != 1 {
		t.Errorf("dead transport closed %d times, want exactly 1", got)
	}
}

// TestManager_DialReusesLiveClientNoRedial proves the inverse contract: a
// healthy client serves the dial directly and never triggers reconnect.
// A redial here would mean Dial tears down working transports.
func TestManager_DialReusesLiveClientNoRedial(t *testing.T) {
	var redials int64
	m := &Manager{
		addr: "x:22",
		log:  discardLogger(),
		dial: func() (sshConn, error) {
			atomic.AddInt64(&redials, 1)
			return &liveConn{}, nil
		},
		dialContext: func(context.Context) (sshConn, error) {
			atomic.AddInt64(&redials, 1)
			return &liveConn{}, nil
		},
	}
	m.client = &liveConn{}

	conn, err := m.Dial("tcp", "127.0.0.1:3000")
	if err != nil {
		t.Fatalf("Dial on healthy client returned error %v, want nil", err)
	}
	if conn == nil {
		t.Fatal("Dial returned nil conn on healthy client")
	}
	if got := atomic.LoadInt64(&redials); got != 0 {
		t.Errorf("redials = %d, want 0 (a healthy client must not trigger reconnect)", got)
	}
}
