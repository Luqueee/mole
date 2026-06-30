package tunnel

import (
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

func (l *liveConn) NewSession() (*ssh.Session, error) {
	return nil, errors.New("unused")
}

func (l *liveConn) Close() error { return nil }

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
