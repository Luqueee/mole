
// Package clip shares a single clipboard image between two mole
// processes over HTTP. The Mac runs Server; the LXC runs Client.
//
// The transport is a plain HTTP endpoint reachable over a private WireGuard
// or Tailscale
// link, not over mole's SSH tunnel: an earlier version of this code
// tried to wire a reverse forward through tunnel.Manager, but
// golang.org/x/crypto/ssh does not expose the ListenOn primitive needed
// to open a remote-side listener, and rolling the tcpip-forward
// message by hand was rejected as fragile for what is, at heart, a
// one-shot screenshot push. WireGuard already gives us a private
// routed path between the two hosts, so we use it directly.
package clip

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// MaxImageBytes caps the size of a single image the server will accept.
// A 32 MiB ceiling is comfortably above any reasonable screenshot while
// still rejecting accidental bulk uploads before they fill the disk.
const MaxImageBytes = 32 << 20
// ErrUnavailable signals a feature is not available on this OS. The
// clip watcher uses it on non-Darwin builds; declaring it in a
// platform-agnostic file lets callers (cmd/mole/clip.go) reference
// it without a build tag of their own.
var ErrUnavailable = errors.New("clip: not supported on this OS")


// DefaultCachePath is the single-slot file the server writes the most
// recent image to. Last writer wins. The mtime is the "is this new?"
// signal Client.Watch uses.
func DefaultCachePath() string {
	return filepath.Join(os.TempDir(), "mole-clip-latest.png")
}

// Server is the clipboard HTTP endpoint. Bind it on the Mac, point the
// LXC's Client at its URL, and Pull/Latest will round-trip PNG bytes.
type Server struct {
	log       *slog.Logger
	cachePath string
}

// New returns a Server that writes accepted images to cachePath (or
// DefaultCachePath when empty) and logs through log (or slog.Default).
func New(log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{log: log, cachePath: DefaultCachePath()}
}

// NewWithCachePath is New plus an explicit cache file path. Useful
// for tests and for setups that want the cache somewhere other
// than os.TempDir. An empty path falls back to DefaultCachePath.
func NewWithCachePath(log *slog.Logger, cachePath string) *Server {
	s := New(log)
	if cachePath != "" {
		s.cachePath = cachePath
	}
	return s
}

// CachePath returns the on-disk file the most recently accepted image
// was written to. Exposed so tests and the watcher can inspect mtime.
func (s *Server) CachePath() string { return s.cachePath }

// Handler returns the http.Handler exposing the server. Mount it on a
// mux or pass it directly to http.Server.
//
//   PUT /clip        raw image/png body, written to the cache file
//   GET  /clip/latest the last cached image, or 404 if nothing yet
//   GET  /clip        alias for /clip/latest
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /clip", s.handlePut)
	mux.HandleFunc("GET /clip/latest", s.handleLatest)
	mux.HandleFunc("GET /clip", s.handleLatest)
	return mux
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	if ct != "" && ct != "image/png" {
		http.Error(w, "only image/png is accepted", http.StatusUnsupportedMediaType)
		return
	}

	// http.MaxBytesReader is the right tool here: it returns a 413
	// automatically when the body exceeds the cap, and it short-
	// circuits further reads so a malicious client can't pump the
	// limit and a byte beyond.
	r.Body = http.MaxBytesReader(w, r.Body, MaxImageBytes)
	defer r.Body.Close()

	tmp, err := os.CreateTemp(filepath.Dir(s.cachePath), "mole-clip-put-*.png")
	if err != nil {
		s.log.Warn("clip server: temp create failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer func() {
		// On any failure path, make sure we don't leave a half-written
		// tmp behind. A successful rename already moved it away.
		_ = os.Remove(tmpName)
	}()

	n, err := io.Copy(tmp, r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
			return
		}
		_ = tmp.Close()
		s.log.Warn("clip server: read body failed", "err", err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if err := tmp.Close(); err != nil {
		s.log.Warn("clip server: tmp close failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Atomic publish: rename tmp over the cache slot. On POSIX this is
	// atomic at the directory level, so a concurrent GET either sees
	// the old file or the new one, never a half-written one.
	if err := os.Rename(tmpName, s.cachePath); err != nil {
		s.log.Warn("clip server: rename failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.log.Info("clipped image", "bytes", n)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	st, err := os.Stat(s.cachePath)
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "no image yet", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Warn("clip server: stat failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	f, err := os.Open(s.cachePath)
	if err != nil {
		s.log.Warn("clip server: open failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
	if _, err := io.Copy(w, f); err != nil {
		// The client probably went away mid-stream; nothing to do but log.
		s.log.Debug("clip server: write to client failed", "err", err)
	}
}

// LatestModTime returns the mtime of the cached image, or the zero
// time if no image has been received yet. Client.Watch uses this as a
// cheap "anything new?" check before doing a full GET.
func (s *Server) LatestModTime() (time.Time, error) {
	st, err := os.Stat(s.cachePath)
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}
