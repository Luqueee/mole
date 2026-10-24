package transfer

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Offer is what a sender proposes before any byte moves.
type Offer struct {
	Peer  string
	To    string
	Files []FileInfo
	Total int64
}

// Pending is an offer waiting for the user's decision.
type Pending struct {
	ID    uint64     `json:"id"`
	Peer  string     `json:"peer"`
	To    string     `json:"to,omitempty"`
	Files []FileInfo `json:"files"`
	Total int64      `json:"total"`
	AtMs  int64      `json:"at_ms"`
}

// ErrRejected is returned to the sender when the user declines an offer.
var ErrRejected = errors.New("rejected by the user")

// ErrNoPending means no offer with that ID is waiting.
var ErrNoPending = errors.New("no such pending transfer")

type gateItem struct {
	p  Pending
	ch chan bool
}

// Gate holds incoming offers until the user accepts or rejects them. Its
// Approve method is a Receiver.Approve.
type Gate struct {
	// Timeout is how long an offer waits for an answer (default 2 minutes).
	Timeout time.Duration
	// Max bounds the queue so a flood of offers cannot pile up (default 16).
	Max int

	mu    sync.Mutex
	next  uint64
	items map[uint64]*gateItem
}

// NewGate returns an empty Gate.
func NewGate() *Gate { return &Gate{items: map[uint64]*gateItem{}} }

// Approve blocks until the offer is decided, expires, or ctx ends.
func (g *Gate) Approve(ctx context.Context, o Offer) error {
	max, timeout := g.Max, g.Timeout
	if max <= 0 {
		max = 16
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	g.mu.Lock()
	if len(g.items) >= max {
		g.mu.Unlock()
		return errors.New("too many transfers waiting for approval")
	}
	g.next++
	id := g.next
	it := &gateItem{ch: make(chan bool, 1), p: Pending{ID: id, Peer: o.Peer, To: o.To, Files: o.Files, Total: o.Total, AtMs: time.Now().UnixMilli()}}
	g.items[id] = it
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.items, id)
		g.mu.Unlock()
	}()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case ok := <-it.ch:
		if ok {
			return nil
		}
		return ErrRejected
	case <-t.C:
		return errors.New("nobody answered: the offer expired")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// List returns the offers waiting, oldest first.
func (g *Gate) List() []Pending {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Pending, 0, len(g.items))
	for _, it := range g.items {
		out = append(out, it.p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Decide answers a waiting offer.
func (g *Gate) Decide(id uint64, accept bool) error {
	g.mu.Lock()
	it, ok := g.items[id]
	g.mu.Unlock()
	if !ok {
		return ErrNoPending
	}
	select {
	case it.ch <- accept:
	default: // already decided
	}
	return nil
}
