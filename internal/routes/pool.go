package routes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/Luqueee/mole/internal/proxy"
)

// NodeConn is one open tunnel to a node.
type NodeConn interface {
	Dial(network, addr string) (net.Conn, error)
	Close() error
}

// Opener opens the tunnel to a registered node. It may block while connecting.
type Opener func(node string) (NodeConn, error)

// retryAfter is how long a node that failed to connect is left alone, so a
// page with many sub-requests to a dead node does not redial for each.
const retryAfter = 5 * time.Second

// Pool is the NodeDialer of the mesh: the primary remote is always there, and
// every other node gets its own tunnel the first time something dials it. A
// tunnel is kept (and kept alive by its own watcher) until Close.
type Pool struct {
	primary proxy.Dialer
	open    Opener
	log     *slog.Logger
	now     func() time.Time // swappable in tests

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	nodes  map[string]*nodeEntry
	closed bool
}

type nodeEntry struct {
	ready chan struct{} // closed once the first open attempt finished
	conn  NodeConn
	err   error
	at    time.Time // when that attempt finished
}

// NewPool returns a Pool whose primary remote is dialed through primary and
// whose other nodes are opened with open.
func NewPool(primary proxy.Dialer, open Opener, log *slog.Logger) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{primary: primary, open: open, log: log, now: time.Now, ctx: ctx, cancel: cancel, nodes: map[string]*nodeEntry{}}
}

// DialNode implements NodeDialer.
func (p *Pool) DialNode(node, network, addr string) (net.Conn, error) {
	if node == "" {
		return p.primary.Dial(network, addr)
	}
	c, err := p.conn(node)
	if err != nil {
		return nil, err
	}
	return c.Dial(network, addr)
}

// Context is done once the pool is closed; node tunnels use it to stop their
// keep-alive loops.
func (p *Pool) Context() context.Context { return p.ctx }

func (p *Pool) conn(node string) (NodeConn, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("routes: pool closed")
		}
		e, ok := p.nodes[node]
		if !ok {
			e = &nodeEntry{ready: make(chan struct{})}
			p.nodes[node] = e
			p.mu.Unlock()
			p.log.Info("opening tunnel to node", "node", node)
			e.conn, e.err = p.open(node)
			e.at = p.now()
			if e.err != nil {
				p.log.Warn("tunnel to node failed", "node", node, "err", e.err)
			}
			close(e.ready)
			return e.conn, wrap(node, e.err)
		}
		// e's fields are written before ready is closed: read them only after.
		if isReady(e) {
			if e.err != nil && p.now().Sub(e.at) >= retryAfter {
				delete(p.nodes, node) // forget the failure and try again
				p.mu.Unlock()
				continue
			}
			p.mu.Unlock()
			return e.conn, wrap(node, e.err)
		}
		p.mu.Unlock()
		<-e.ready // another caller is connecting: share its result
		return e.conn, wrap(node, e.err)
	}
}

func isReady(e *nodeEntry) bool {
	select {
	case <-e.ready:
		return true
	default:
		return false
	}
}

func wrap(node string, err error) error {
	if err != nil {
		return fmt.Errorf("node %s: %w", node, err)
	}
	return nil
}

// Close closes every node tunnel.
func (p *Pool) Close() {
	p.cancel()
	p.mu.Lock()
	p.closed = true
	nodes := p.nodes
	p.nodes = map[string]*nodeEntry{}
	p.mu.Unlock()
	for _, e := range nodes {
		<-e.ready
		if e.conn != nil {
			_ = e.conn.Close()
		}
	}
}
