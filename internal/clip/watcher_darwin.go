//go:build darwin

package clip

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"time"
)

// Watcher polls the macOS clipboard for PNG images and pushes each new
// one to a clip.Server's PUT /clip endpoint. It uses osascript +
// xxd instead of CGO so the binary stays statically linkable across
// macOS versions.
type Watcher struct {
	log    *slog.Logger
	lastTS string // last seen SHA; "" means "never seen"
}

// NewWatcher returns a Watcher ready to be Run.
func NewWatcher(log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{log: log}
}

// Run blocks until ctx is done, polling the clipboard every interval
// (500 ms is a good default) and PUTting each new PNG to endpoint.
//
// The command we run is:
//
//	osascript -e 'the clipboard as «class PNGf»' | xxd -r -p
//
// `the clipboard as «class PNGf»` returns the PNG bytes hex-encoded
// in NSPasteboard's four-char-code format; xxd -r -p reverses that.
// Both osascript and xxd ship with macOS.
//
// If the clipboard holds text (or anything that isn't a PNG), the
// command exits 0 with empty output; we treat that as "no change".
func (w *Watcher) Run(ctx context.Context, endpoint string, interval time.Duration) error {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	if endpoint == "" {
		return errors.New("clip watcher: empty endpoint")
	}

	// Probe osascript and xxd up front — fail fast on a misconfigured
	// Mac rather than failing every tick.
	if _, err := exec.LookPath("osascript"); err != nil {
		return fmt.Errorf("clip watcher: osascript not on PATH: %w", err)
	}
	if _, err := exec.LookPath("xxd"); err != nil {
		return fmt.Errorf("clip watcher: xxd not on PATH: %w", err)
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	w.log.Info("clip watcher started", "endpoint", endpoint, "interval", interval.String())
	defer w.log.Info("clip watcher stopped")

	// Run an initial tick immediately so we don't wait `interval`
	// after startup before checking the clipboard.
	if err := w.tick(ctx, endpoint); err != nil && !errors.Is(err, context.Canceled) {
		w.log.Warn("clip watcher tick failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := w.tick(ctx, endpoint); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Warn("clip watcher tick failed", "err", err)
			}
		}
	}
}

func (w *Watcher) tick(ctx context.Context, endpoint string) error {
	data, err := readPasteboardPNG(ctx)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		// Empty clipboard, or text, or something we can't parse.
		// Either way: nothing to push.
		return nil
	}

	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	if sum == w.lastTS {
		w.log.Debug("clipboard unchanged")
		return nil
	}

	// PUT the raw bytes. 30s timeout — generous because Watcher tick
	// can fire while a slow server is processing a previous PUT.
	putCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(putCtx, http.MethodPut, endpoint+"/clip", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "image/png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("clip watcher: server returned %s: %s", resp.Status, body)
	}

	w.lastTS = sum
	w.log.Info("clipped image", "bytes", len(data))
	return nil
}

// readPasteboardPNG shells out to osascript and xxd and returns the
// raw PNG bytes. An empty slice means "no PNG on the clipboard right
// now" and is not an error.
func readPasteboardPNG(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", `osascript -e 'the clipboard as «class PNGf»' | xxd -r -p`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// osascript exits non-zero when the clipboard doesn't contain
		// the requested type. That's the common case (text clipboard)
		// — treat as "empty" and don't spam the log.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, nil
		}
		return nil, fmt.Errorf("clip watcher: read pasteboard: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.Bytes(), nil
}
