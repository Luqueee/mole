package discover

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDialer simulates a remote that has certain ports open. All
// unknown ports fail with errFakeClosed.
type fakeDialer struct {
	mu       sync.Mutex
	open     map[int]bool
	dials    atomic.Int64
	failWith error
}

var errFakeClosed = errors.New("fake: port closed")

func newFakeDialer(open []int) *fakeDialer {
	f := &fakeDialer{open: make(map[int]bool), failWith: errFakeClosed}
	for _, p := range open {
		f.open[p] = true
	}
	return f
}

func (f *fakeDialer) Dial(network, addr string) (net.Conn, error) {
	f.dials.Add(1)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if host != "127.0.0.1" {
		return nil, errors.New("fake: only 127.0.0.1 supported")
	}
	var p int
	if _, err := scanInt(port, &p); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open[p] {
		return nil, f.failWith
	}
	// Return a connected pipe that the test can close.
	c1, c2 := net.Pipe()
	_ = c2.Close() // caller will close c1; this is fine for the smoke test.
	return c1, nil
}

func (f *fakeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.Dial(network, addr)
}

type sweepDialer struct {
	inner     *fakeDialer
	delay     time.Duration
	dials     atomic.Int64
	active    atomic.Int64
	maxActive atomic.Int64
	closed    atomic.Int64
}

func (d *sweepDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	active := d.active.Add(1)
	defer d.active.Add(-1)
	updateMax(&d.maxActive, active)
	d.dials.Add(1)
	if d.delay > 0 {
		timer := time.NewTimer(d.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return d.inner.DialContext(ctx, network, addr)
}

func (d *sweepDialer) Close() error {
	d.closed.Add(1)
	return nil
}

func updateMax(max *atomic.Int64, value int64) {
	for {
		current := max.Load()
		if value <= current || max.CompareAndSwap(current, value) {
			return
		}
	}
}

// scanInt is a tiny helper to avoid importing strconv in the fake.
func scanInt(s string, out *int) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch < '0' || ch > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(ch-'0')
	}
	*out = n
	return len(s), nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestProbe_AllClosed(t *testing.T) {
	d := newFakeDialer(nil)
	got := Probe(context.Background(), d, []int{3000, 5173, 8080}, quietLogger())
	if len(got) != 0 {
		t.Errorf("Probe with no open ports = %v, want empty", got)
	}
	if d.dials.Load() != 3 {
		t.Errorf("dials = %d, want 3", d.dials.Load())
	}
}

func TestProbe_AllOpen(t *testing.T) {
	d := newFakeDialer([]int{3000, 5173, 8080})
	got := Probe(context.Background(), d, []int{3000, 5173, 8080}, quietLogger())
	sort.Ints(got)
	want := []int{3000, 5173, 8080}
	if !equalInts(got, want) {
		t.Errorf("Probe = %v, want %v", got, want)
	}
}

func TestProbe_Mixed(t *testing.T) {
	d := newFakeDialer([]int{3000, 8080})
	got := Probe(context.Background(), d, []int{3000, 5173, 8080, 9000}, quietLogger())
	sort.Ints(got)
	want := []int{3000, 8080}
	if !equalInts(got, want) {
		t.Errorf("Probe = %v, want %v", got, want)
	}
}

func TestProbe_EmptyCandidates(t *testing.T) {
	d := newFakeDialer([]int{3000})
	got := Probe(context.Background(), d, nil, quietLogger())
	if len(got) != 0 {
		t.Errorf("Probe(nil) = %v, want empty", got)
	}
	if d.dials.Load() != 0 {
		t.Errorf("dials = %d, want 0", d.dials.Load())
	}
}

func TestProbeWithFactory_ReusesOneTransportAndBoundsConcurrency(t *testing.T) {
	candidates := []int{3000, 3001, 3002, 3003, 3004, 3005, 3006, 3007, 3008, 3009, 3010, 3011}
	d := &sweepDialer{
		inner: newFakeDialer(candidates),
		delay: time.Millisecond,
	}
	var factories atomic.Int64

	got := ProbeWithFactory(context.Background(), func(context.Context) (SweepDialer, error) {
		factories.Add(1)
		return d, nil
	}, candidates, quietLogger())
	sort.Ints(got)

	if !equalInts(got, candidates) {
		t.Fatalf("ProbeWithFactory = %v, want %v", got, candidates)
	}
	if got := factories.Load(); got != 1 {
		t.Errorf("factory calls = %d, want 1", got)
	}
	if got := d.dials.Load(); got != int64(len(candidates)) {
		t.Errorf("dials = %d, want %d", got, len(candidates))
	}
	if got := d.maxActive.Load(); got > maxProbeConcurrency {
		t.Errorf("max concurrent probes = %d, want at most %d", got, maxProbeConcurrency)
	}
	if got := d.closed.Load(); got != 1 {
		t.Errorf("transport closes = %d, want 1", got)
	}
}

type blockingDialer struct {
	started chan struct{}
	once    sync.Once
}

func (d *blockingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.once.Do(func() { close(d.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestProbe_CancelsStalledDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &blockingDialer{started: make(chan struct{})}
	done := make(chan []int, 1)
	go func() {
		done <- Probe(ctx, d, []int{3000}, quietLogger())
	}()

	select {
	case <-d.started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	cancel()

	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("Probe after cancellation = %v, want empty", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Probe did not return after cancellation")
	}
}

func TestProbe_TimeoutsStalledDial(t *testing.T) {
	d := &blockingDialer{started: make(chan struct{})}
	start := time.Now()
	got := probeWithTimeout(context.Background(), d, []int{3000}, quietLogger(), 10*time.Millisecond)
	if len(got) != 0 {
		t.Errorf("Probe after timeout = %v, want empty", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Probe timeout took %s, want it bounded", elapsed)
	}
}

func TestProbe_RealLoopbackServer(t *testing.T) {
	// Bind a real TCP listener on an ephemeral port and have the fake
	// dialer report it as open. This exercises the real net.Conn return
	// path more thoroughly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	openPort := ln.Addr().(*net.TCPAddr).Port

	// Also start a listener on a port we will NOT report as open.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen2: %v", err)
	}
	defer ln2.Close()
	closedPort := ln2.Addr().(*net.TCPAddr).Port

	dialer := dialerFunc(func(network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if host != "127.0.0.1" {
			return nil, errors.New("bad host")
		}
		var p int
		for i := 0; i < len(port); i++ {
			ch := port[i]
			if ch < '0' || ch > '9' {
				return nil, errors.New("bad port")
			}
			p = p*10 + int(ch-'0')
		}
		if p == openPort {
			return net.Dial("tcp", addr)
		}
		return nil, errors.New("fake: closed")
	})

	got := Probe(context.Background(), dialer, []int{openPort, closedPort, 65530}, quietLogger())
	sort.Ints(got)
	want := []int{openPort}
	if !equalInts(got, want) {
		t.Errorf("Probe = %v, want %v", got, want)
	}
}

type dialerFunc func(network, addr string) (net.Conn, error)

func (f dialerFunc) Dial(network, addr string) (net.Conn, error) {
	return f(network, addr)
}

func (f dialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f(network, addr)
}

func TestProbe_ParallelSafety(t *testing.T) {
	// Run two Probe calls in parallel against a shared dialer that
	// tracks concurrent calls; the implementation must be goroutine-safe.
	d := newFakeDialer([]int{3000, 5173, 8080})

	var wg sync.WaitGroup
	const K = 10
	results := make([][]int, K)
	for i := 0; i < K; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = Probe(context.Background(), d, []int{3000, 5173, 8080, 9000, 9090}, quietLogger())
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if len(r) != 3 {
			t.Errorf("results[%d] = %v, want length 3", i, r)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
