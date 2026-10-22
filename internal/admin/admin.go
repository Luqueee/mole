// Package admin exposes a tiny HTTP server for runtime introspection:
// /status returns JSON stats + caller-supplied info, /health is a
// 200 OK liveness probe.
package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrConflict marks a controller error caused by a client-side port
// conflict, such as an excluded or already-forwarded port. Other
// controller errors are operational failures and map to HTTP 500.
var ErrConflict = errors.New("admin: port conflict")

// Stats tracks runtime counters for the admin endpoint. Safe for
// concurrent use.
type Stats struct {
	StartedAt time.Time

	evMu   sync.Mutex
	events []Event // ring of the most recent connections, oldest first
	nextID uint64

	activeConns atomic.Int64
	totalConns  atomic.Uint64
	failedDials atomic.Uint64
}

// Event is one finished forwarded connection, as served by GET /events.
type Event struct {
	ID       uint64 `json:"id"`
	Port     int    `json:"port"`
	StartMs  int64  `json:"start_ms"`
	DurMs    int64  `json:"dur_ms"`
	BytesOut int64  `json:"bytes_out"`
	BytesIn  int64  `json:"bytes_in"`
	Failed   bool   `json:"failed"`
}

const maxEvents = 256

// Record appends a finished connection to the event ring, assigning its ID.
func (s *Stats) Record(ev Event) {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	s.nextID++
	ev.ID = s.nextID
	if len(s.events) >= maxEvents {
		s.events = append(s.events[:0], s.events[1:]...)
	}
	s.events = append(s.events, ev)
}

// EventsSince returns the events with ID greater than since, and the
// newest ID issued so far (so a poller can resume from it).
func (s *Stats) EventsSince(since uint64) ([]Event, uint64) {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	out := []Event{}
	for _, ev := range s.events {
		if ev.ID > since {
			out = append(out, ev)
		}
	}
	return out, s.nextID
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

// New creates an admin Server. extra is returned in /status under
// "info" so callers can add their own fields (ports, remote, etc.).
func New(stats *Stats, extra map[string]any) *Server {
	return &Server{stats: stats, extra: extra}
}
// Server is a tiny HTTP server exposing /status and /health.
type Server struct {
	stats   *Stats
	extra   map[string]any
	portsFn func() []int
	portsCtl PortController // optional: live-mutate the discover-port set
}

// PortController is the live-mutation counterpart of WithPorts. When
// set, POST/DELETE on /ports/discover lets `mole ports add/remove`
// apply changes without restarting the forwarder.
//
// AddDiscover returns a non-nil error if the port is excluded or
// already forwarded; the handler maps those to 409. RemoveDiscover
// returns nil if the port wasn't there — a delete is idempotent.
type PortController interface {
	AddDiscover(port int) error
	RemoveDiscover(port int) error
}
// WithPorts registers a callback returning the currently forwarded
// ports. When set, /status reports them live under info.ports —
// useful when the port set changes at runtime (periodic
// auto-discovery). The callback must be safe for concurrent use;
// it's invoked per request.
func (s *Server) WithPorts(fn func() []int) *Server {
	s.portsFn = fn
	return s
}

// WithPortController registers a callback for live port-mutation.
// The callback must be safe for concurrent use; it's invoked per
// HTTP request.
func (s *Server) WithPortController(ctl PortController) *Server {
	s.portsCtl = ctl
	return s
}

// Handler returns the HTTP handler for the admin endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/events", s.handleEvents)
	if s.portsCtl != nil {
		mux.HandleFunc("POST /ports/discover", s.handlePortAdd)
		mux.HandleFunc("DELETE /ports/discover/", s.handlePortDelete)
	}
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

// handleEvents serves GET /events?since=N: the connections finished after
// event N, plus the newest ID to poll from next time.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	evs, last := s.stats.EventsSince(since)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": evs, "last": last})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// portRequest is the JSON body for POST /ports/discover. We only
// need the port number; admin_addr/port-mutation are out of scope
// for now (Pinned ports, once added, would be a different endpoint).
type portRequest struct {
	Port int `json:"port"`
}

// handlePortAdd accepts a JSON body { "port": N } and asks the
// PortController to start forwarding it. Responses:
//
//	201 Created    port added
//	400 Bad Request invalid port or malformed body
//	409 Conflict   port is excluded or already forwarded
//	500 Internal   controller errored
func (s *Server) handlePortAdd(w http.ResponseWriter, r *http.Request) {
	var req portRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		http.Error(w, "port out of range (1-65535)", http.StatusBadRequest)
		return
	}
	if err := s.portsCtl.AddDiscover(req.Port); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrConflict) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"status":"added"}`))
}

// handlePortDelete accepts DELETE /ports/discover/{port} and asks
// the PortController to stop forwarding it. Idempotent: a missing
// port returns 200 with a note in the body, not 404, so a stale
// client doesn't have to special-case it.
func (s *Server) handlePortDelete(w http.ResponseWriter, r *http.Request) {
	portStr := strings.TrimPrefix(r.URL.Path, "/ports/discover/")
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "invalid port in path", http.StatusBadRequest)
		return
	}
	if err := s.portsCtl.RemoveDiscover(port); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"removed"}`))
}
