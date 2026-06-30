package main

import (
	"io"
	"log/slog"
	"net"
	"testing"
)

// newRetainForwarder builds a forwarder with empty maps and a discard
// logger. retain calls f.log.Info on every prune, so log must be
// non-nil; it never touches mgr, so mgr stays nil.
func newRetainForwarder() *forwarder {
	return &forwarder{
		mgr:         nil,
		remoteLabel: "dev",
		log:         slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		active:      map[int]net.Listener{},
		failed:      map[int]bool{},
		pinned:      map[int]bool{},
	}
}

// openLoopback opens a real listener on an ephemeral loopback port and
// registers it in f.active under its real port number — a meaningful key
// retain compares against keep/pinned. It returns the port and the
// listener so callers can assert it was closed. A cleanup closes the
// listener so retained ones don't leak; double-closing a pruned one is
// harmless.
func openLoopback(t *testing.T, f *forwarder) (int, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if _, dup := f.active[port]; dup {
		t.Fatalf("ephemeral port %d collided with an already-registered listener", port)
	}
	f.active[port] = ln
	t.Cleanup(func() { _ = ln.Close() })
	return port, ln
}

func contains(ports []int, want int) bool {
	for _, p := range ports {
		if p == want {
			return true
		}
	}
	return false
}

// TestForwarderRetain_PrunesAbsentPorts: a port no longer live on the
// remote is closed and dropped, while a still-live one is kept.
func TestForwarderRetain_PrunesAbsentPorts(t *testing.T) {
	f := newRetainForwarder()
	pa, _ := openLoopback(t, f)
	pb, lnb := openLoopback(t, f)
	pc, lnc := openLoopback(t, f)

	f.retain(map[int]bool{pa: true})

	got := f.ports() // ports() locks f.mu itself; we hold no lock here.
	if len(got) != 1 || !contains(got, pa) {
		t.Fatalf("after retain(keep={%d}): ports() = %v, want only [%d]", pa, got, pa)
	}
	if contains(got, pb) {
		t.Errorf("absent port %d still active: ports=%v", pb, got)
	}
	if contains(got, pc) {
		t.Errorf("absent port %d still active: ports=%v", pc, got)
	}

	// A pruned listener must have been closed: a second Close returns a
	// non-nil error because the underlying fd is already gone.
	if err := lnb.Close(); err == nil {
		t.Errorf("pruned listener for port %d was not closed (second Close returned nil)", pb)
	}
	if err := lnc.Close(); err == nil {
		t.Errorf("pruned listener for port %d was not closed (second Close returned nil)", pc)
	}
}

// TestForwarderRetain_PinnedSurvivesEmptyKeep: a pinned port is never
// pruned, even when the remote reports nothing live (keep is empty),
// while an unpinned port in the same set is dropped and closed.
func TestForwarderRetain_PinnedSurvivesEmptyKeep(t *testing.T) {
	f := newRetainForwarder()
	pa, lna := openLoopback(t, f)
	pb, _ := openLoopback(t, f)
	f.pinned[pb] = true

	f.retain(map[int]bool{}) // remote reports nothing live

	got := f.ports()
	if contains(got, pa) {
		t.Errorf("unpinned absent port %d survived prune: ports=%v", pa, got)
	}
	if !contains(got, pb) {
		t.Errorf("pinned port %d was pruned despite empty keep: ports=%v", pb, got)
	}
	if err := lna.Close(); err == nil {
		t.Errorf("pruned listener for port %d was not closed (second Close returned nil)", pa)
	}
}

// TestForwarderRetain_KeepAllIsNoOp: when every active port is still
// live, retain closes nothing and the active set is unchanged.
func TestForwarderRetain_KeepAllIsNoOp(t *testing.T) {
	f := newRetainForwarder()
	pa, _ := openLoopback(t, f)
	pb, _ := openLoopback(t, f)

	f.retain(map[int]bool{pa: true, pb: true})

	got := f.ports()
	if len(got) != 2 {
		t.Fatalf("after keep-all retain: ports() = %v, want 2 entries", got)
	}
	if !contains(got, pa) || !contains(got, pb) {
		t.Errorf("retain pruned a kept port: ports=%v, want both %d and %d", got, pa, pb)
	}
}
