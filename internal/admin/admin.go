// Package admin exposes a tiny HTTP server for runtime introspection:
// /status returns JSON stats + caller-supplied info, /health is a
// 200 OK liveness probe.
package admin

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// Stats tracks runtime counters for the admin endpoint. Safe for
// concurrent use.
type Stats struct {
	StartedAt time.Time

	activeConns atomic.Int64
	totalConns  atomic.Uint64
	failedDials atomic.Uint64
}

// NewStats returns a Stats with StartedAt set to now.
func NewStats() *Stats {
	return &Stats{StartedAt: time.Now()}
}

// OnConnect is called when a new client connection is accepted.
func (s *Stats) OnConnect() {
	s.activeConns.Add(1)
	s.totalConns.Add(1)
}

// OnDisconnect is called when a client connection ends.
func (s *Stats) OnDisconnect() {
	s.activeConns.Add(-1)
}

// OnDialFail is called when a dial to the remote fails.
func (s *Stats) OnDialFail() {
	s.failedDials.Add(1)
}

type snapshot struct {
	Uptime      string `json:"uptime"`
	ActiveConns int64  `json:"active_conns"`
	TotalConns  uint64 `json:"total_conns"`
	FailedDials uint64 `json:"failed_dials"`
}

// Server is a tiny HTTP server exposing /status and /health.
type Server struct {
	stats   *Stats
	extra   map[string]any
	portsFn func() []int
}

// New creates an admin Server. extra is returned in /status under
// "info" so callers can add their own fields (ports, remote, etc.).
func New(stats *Stats, extra map[string]any) *Server {
	return &Server{stats: stats, extra: extra}
}

// WithPorts registers a callback returning the currently forwarded
// ports. When set, /status reports them live under info.ports — useful
// when the port set changes at runtime (periodic auto-discovery). The
// callback must be safe for concurrent use; it's invoked per request.
func (s *Server) WithPorts(fn func() []int) *Server {
	s.portsFn = fn
	return s
}

// Handler returns the HTTP handler for the admin endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/health", s.handleHealth)
	return mux
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	snap := snapshot{
		Uptime:      time.Since(s.stats.StartedAt).Truncate(time.Second).String(),
		ActiveConns: s.stats.activeConns.Load(),
		TotalConns:  s.stats.totalConns.Load(),
		FailedDials: s.stats.failedDials.Load(),
	}
	info := s.extra
	if s.portsFn != nil {
		// Overlay live ports without mutating the shared extra map.
		merged := make(map[string]any, len(s.extra)+1)
		for k, v := range s.extra {
			merged[k] = v
		}
		merged["ports"] = s.portsFn()
		info = merged
	}
	out := map[string]any{
		"stats": snap,
		"info":  info,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
