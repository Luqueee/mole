package routes

import (
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeConn struct {
	dials  atomic.Int32
	closed atomic.Bool
}

func (c *fakeConn) Dial(network, addr string) (net.Conn, error) {
	c.dials.Add(1)
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}
func (c *fakeConn) Close() error { c.closed.Store(true); return nil }

type primary struct{ dials atomic.Int32 }

func (p *primary) Dial(network, addr string) (net.Conn, error) {
	p.dials.Add(1)
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}

func newTestPool(open Opener) (*Pool, *primary) {
	pr := &primary{}
	return NewPool(pr, open, slog.New(slog.DiscardHandler)), pr
}

func TestPoolPrimaryNeverOpensANode(t *testing.T) {
	opened := 0
	p, pr := newTestPool(func(string) (NodeConn, error) { opened++; return &fakeConn{}, nil })
	for range 3 {
		if _, err := p.DialNode("", "tcp", "127.0.0.1:1"); err != nil {
			t.Fatal(err)
		}
	}
	if opened != 0 || pr.dials.Load() != 3 {
		t.Errorf("opened=%d primary dials=%d", opened, pr.dials.Load())
	}
}

func TestPoolOpensEachNodeOnceEvenUnderConcurrency(t *testing.T) {
	var opened atomic.Int32
	gate := make(chan struct{})
	conns := map[string]*fakeConn{"a": {}, "b": {}}
	p, _ := newTestPool(func(n string) (NodeConn, error) {
		opened.Add(1)
		<-gate // hold the first open so the others pile up behind it
		return conns[n], nil
	})
	var wg sync.WaitGroup
	for i := range 20 {
		node := []string{"a", "b"}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.DialNode(node, "tcp", "x:1"); err != nil {
				t.Errorf("DialNode(%s): %v", node, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if opened.Load() != 2 {
		t.Errorf("opened %d tunnels, want 2 (one per node)", opened.Load())
	}
	if conns["a"].dials.Load() != 10 || conns["b"].dials.Load() != 10 {
		t.Errorf("dials a=%d b=%d, want 10 each", conns["a"].dials.Load(), conns["b"].dials.Load())
	}
}

func TestPoolRetriesAFailedNodeOnlyAfterTheWindow(t *testing.T) {
	var attempts atomic.Int32
	fail := atomic.Bool{}
	fail.Store(true)
	p, _ := newTestPool(func(string) (NodeConn, error) {
		attempts.Add(1)
		if fail.Load() {
			return nil, errors.New("connection refused")
		}
		return &fakeConn{}, nil
	})
	now := time.Now()
	p.now = func() time.Time { return now }

	if _, err := p.DialNode("a", "tcp", "x:1"); err == nil || err.Error() != "node a: connection refused" {
		t.Fatalf("first dial: %v", err)
	}
	// a burst of sub-requests inside the window must not redial
	for range 5 {
		if _, err := p.DialNode("a", "tcp", "x:1"); err == nil {
			t.Fatal("dial through a failed node succeeded")
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts inside the window = %d, want 1", attempts.Load())
	}

	fail.Store(false)
	now = now.Add(retryAfter + time.Second)
	if _, err := p.DialNode("a", "tcp", "x:1"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want 2", attempts.Load())
	}
}

func TestPoolCloseClosesTunnelsAndRefusesDials(t *testing.T) {
	c := &fakeConn{}
	p, _ := newTestPool(func(string) (NodeConn, error) { return c, nil })
	if _, err := p.DialNode("a", "tcp", "x:1"); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if !c.closed.Load() {
		t.Error("Close left the node tunnel open")
	}
	if p.Context().Err() == nil {
		t.Error("Close did not cancel the pool context")
	}
	if _, err := p.DialNode("a", "tcp", "x:1"); err == nil {
		t.Error("dial after Close succeeded")
	}
}
