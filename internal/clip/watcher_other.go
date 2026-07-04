//go:build !darwin

package clip

import (
	"context"
	"log/slog"
)

// Watcher is a Darwin-only feature; on other platforms we expose the
// same surface so cmd/mole can compile but every Run returns
// ErrUnavailable (declared in server.go). Splitting the build keeps
// the Linux and Windows binaries working without dragging in
// osascript.
type Watcher struct{}

// NewWatcher returns a Watcher stub on non-Darwin platforms. Run
// always returns ErrUnavailable; callers should detect that and skip
// the watcher goroutine (cmd/mole/clip.go does this).
func NewWatcher(log *slog.Logger) *Watcher { return &Watcher{} }

// Run returns ErrUnavailable immediately. The context is still
// respected so the caller can rely on a quick return.
func (w *Watcher) Run(ctx context.Context, endpoint string, interval time.Duration) error {
	return ErrUnavailable
}
