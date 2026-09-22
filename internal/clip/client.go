package clip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ErrNoImage is returned by Pull when the server has nothing cached
// yet (HTTP 404). Callers can map this to a non-zero exit so the
// shell / Claude Code can tell "no clipboard activity" apart from
// "server is down".
var ErrNoImage = errors.New("clip: no image on server")

// Client talks to a clip.Server over HTTP. The URL is whatever the
// server is reachable on the private WireGuard or Tailscale link — typically
// http://<mac-private-ip>:7777.
type Client struct {
	endpoint string
	log      *slog.Logger
	http     *http.Client
}

// NewClient returns a Client that fetches from endpoint. Pass a nil
// logger to fall back to slog.Default. The HTTP client has a 5s
// timeout by default — long enough for a screenshot, short enough to
// surface server death fast.
func NewClient(endpoint string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		endpoint: endpoint,
		log:      log,
		http:     &http.Client{Timeout: 5 * time.Second},
	}
}

// Pull fetches the latest image from the server and writes it under
// os.TempDir()/mole-clip/mole-clip-<unix-nano>.png. The timestamp
// suffix means repeated pulls don't clobber each other. Returns the
// absolute path of the written file.
func (c *Client) Pull(ctx context.Context) (string, error) {
	path, _, err := c.pull(ctx)
	return path, err
}

// pull is Pull plus the Last-Modified value from the exact GET response.
// Watch uses that value as its watermark so a publication that races with
// the request cannot be skipped by a follow-up HEAD.
func (c *Client) pull(ctx context.Context) (string, time.Time, error) {
	if c.endpoint == "" {
		return "", time.Time{}, errors.New("clip: empty endpoint")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/clip/latest", nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("clip: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("clip: get latest: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusNotFound:
		return "", time.Time{}, ErrNoImage
	default:
		return "", time.Time{}, fmt.Errorf("clip: server returned %s", resp.Status)
	}
	var pulledAt time.Time
	if value := resp.Header.Get("Last-Modified"); value != "" {
		pulledAt, _ = http.ParseTime(value)
	}

	dir := filepath.Join(os.TempDir(), "mole-clip")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", time.Time{}, fmt.Errorf("clip: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, fmt.Sprintf("mole-clip-%d.png", time.Now().UnixNano()))

	tmp, err := os.CreateTemp(dir, "mole-clip-pull-*.png")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("clip: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	// On any error before rename, clean up the tmp so we don't leave
	// debris in the temp dir.
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return "", time.Time{}, fmt.Errorf("clip: read body: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", time.Time{}, fmt.Errorf("clip: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, out); err != nil {
		return "", time.Time{}, fmt.Errorf("clip: rename: %w", err)
	}
	committed = true

	c.log.Info("clipped image pulled", "path", out, "server", c.endpoint)
	return out, pulledAt, nil
}

// LastModified issues a HEAD against the server and returns the
// Last-Modified header parsed as time.Time. Returns the zero time if
// the header is missing or the request fails — callers should treat
// zero as "unknown, force a Pull".
func (c *Client) LastModified(ctx context.Context) (time.Time, error) {
	if c.endpoint == "" {
		return time.Time{}, errors.New("clip: empty endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.endpoint+"/clip/latest", nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, nil
	}
	v := resp.Header.Get("Last-Modified")
	if v == "" {
		return time.Time{}, nil
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// Watch polls the server every interval; when a new image is detected
// (Last-Modified advances past the previously-seen watermark), it
// calls Pull and invokes onNew with the path. Watch returns when ctx
// is done.
//
// onNew is called synchronously: if it blocks, the next poll waits.
// onNew returning an error stops the watch — useful for tests.
func (c *Client) Watch(ctx context.Context, interval time.Duration, onNew func(string) error) error {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	var lastSeen time.Time
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		lm, err := c.LastModified(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			c.log.Debug("clip watch: last-modified failed", "err", err)
		}
		// Only consider a new image if we have a parsed mtime and it
		// has advanced. A zero mtime (header missing) is treated as
		// "indeterminate" — we do nothing until we have a baseline.
		if !lm.IsZero() && lm.After(lastSeen) {
			path, pulledAt, err := c.pull(ctx)
			if errors.Is(err, ErrNoImage) {
				// Server says there's nothing yet; just keep polling.
			} else if err != nil {
				return err
			} else {
				if err := onNew(path); err != nil {
					return err
				}
				// Advance the watermark to the image returned by this
				// GET. A later publication may race with Pull; using a
				// follow-up HEAD here would skip that image.
				if pulledAt.IsZero() {
					pulledAt = lm
				}
				lastSeen = pulledAt
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
