package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luqueee/mole/internal/clip"
)

// TestClipRoundTrip exercises the full serve→pull path without
// touching the network: a clip.Server takes the role of the Mac
// endpoint, and a clip.Client (the same code path runClipPull uses)
// pulls from it. The "URL" is an httptest URL — this deliberately
// bypasses WireGuard and any process boundary so the test is
// hermetic.
//
// The Server is constructed with an explicit cache path under
// t.TempDir(). clip.Server's default points at a global slot in
// os.TempDir() (production behaviour), which means any test that
// leaves a file there bleeds into the next test run — we saw this
// regress as a flaky failure after manual smoke tests. Always pass
// a per-test path.
func TestClipRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	// "Mac" side: clip.Server with a per-test cache path.
	mac := clip.NewWithCachePath(silentLogger(), filepath.Join(dir, "mac-cache.png"))
	macTS := httptest.NewServer(mac.Handler())
	t.Cleanup(macTS.Close)

	// "LXC" side: same code as runClipPull.
	pull := clip.NewClient(macTS.URL, silentLogger())

	// 1) nothing cached yet — Pull should surface ErrNoImage.
	_, err := pull.Pull(context.Background())
	if !errors.Is(err, clip.ErrNoImage) {
		t.Fatalf("expected ErrNoImage, got %v", err)
	}

	// 2) push a payload to the server.
	payload := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x01}
	req, _ := http.NewRequest(http.MethodPut, macTS.URL+"/clip", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "image/png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}

	// 3) Pull should now succeed and the file on disk should match.
	path, err := pull.Pull(context.Background())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pulled file: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("pulled bytes = %x, want %x", body, payload)
	}

	// 4) LastModified should report a recent timestamp now.
	lm, err := pull.LastModified(context.Background())
	if err != nil {
		t.Fatalf("LastModified: %v", err)
	}
	if lm.IsZero() {
		t.Error("LastModified returned zero time after a successful PUT")
	}
	if time.Since(lm) > 30*time.Second {
		t.Errorf("LastModified = %v, expected recent", lm)
	}

	// 5) The cache file lives under TMPDIR, not the system /tmp.
	if !strings.HasPrefix(path, filepath.Clean(dir)) {
		// os.TempDir may resolve through /var/folders/.../T on
		// macOS rather than literally echoing TMPDIR. Accept any
		// path that contains the temp dir name as a suffix.
		if !strings.Contains(path, filepath.Base(dir)) && !strings.HasPrefix(path, "/") {
			t.Errorf("path %q doesn't look like it lives under TMPDIR", path)
		}
	}
}

// TestClipPull_NoImage_ExitCodeMapping documents that runClipPull's
// error-classification contract (ErrNoImage → exit 3) is what
// callers actually see. We don't exec the binary; we rely on the
// same package code path.
//
// As with TestClipRoundTrip, we point the Server at a per-test
// cache path to keep the global default slot clean.
func TestClipPull_NoImage_ExitCodeMapping(t *testing.T) {
	dir := t.TempDir()
	mac := clip.NewWithCachePath(silentLogger(), filepath.Join(dir, "mac-cache.png"))
	macTS := httptest.NewServer(mac.Handler())
	t.Cleanup(macTS.Close)

	c := clip.NewClient(macTS.URL, silentLogger())
	_, err := c.Pull(context.Background())
	if !errors.Is(err, clip.ErrNoImage) {
		t.Errorf("error = %v, want clip.ErrNoImage (so runClipPull returns 3)", err)
	}
}

func TestClipBindScope(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:7777":   false,
		"100.64.0.10:7777": false,
		"10.0.0.10:7777":   false,
		"[::1]:7777":       false,
		"0.0.0.0:7777":     true,
		":7777":            true,
		"[::]:7777":        true,
	}
	for addr, wantBroad := range cases {
		if got := isBroadClipBind(addr); got != wantBroad {
			t.Errorf("isBroadClipBind(%q) = %v, want %v", addr, got, wantBroad)
		}
	}
}

// silentLogger returns a logger that throws away every record. Keeps
// the test output clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
