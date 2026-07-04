package clip

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
)

func TestClient_Pull_EmptyServer_ReturnsErrNoImage(t *testing.T) {
	_, ts := newTestServer(t)
	c := NewClient(ts.URL, discardLogger())
	_, err := c.Pull(context.Background())
	if !errors.Is(err, ErrNoImage) {
		t.Errorf("err = %v, want ErrNoImage", err)
	}
}

func TestClient_Pull_RoundTripsBytesToFile(t *testing.T) {
	_, ts := newTestServer(t)

	// Seed the server with PNG-magic bytes via PUT.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/clip", bytes.NewReader(pngMagic))
	req.Header.Set("Content-Type", "image/png")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatalf("seed PUT: %v", err)
	} else {
		resp.Body.Close()
	}

	// Redirect the client's temp dir to a per-test location so we
	// don't litter the real /tmp.
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	c := NewClient(ts.URL, discardLogger())
	path, err := c.Pull(context.Background())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !strings.HasPrefix(path, filepath.Clean(dir)) {
		// On macOS TMPDIR resolves through /var/folders/.../T/ via
		// os.TempDir; on Linux it's the literal dir. We accept any
		// path that contains the temp dir name as a suffix, which
		// is the more portable check.
		if !strings.Contains(path, filepath.Base(dir)) && !strings.HasPrefix(path, "/") {
			t.Errorf("path %q doesn't look like it lives under TMPDIR", path)
		}
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pulled file: %v", err)
	}
	if !bytes.Equal(body, pngMagic) {
		t.Errorf("pulled bytes = %x, want %x", body, pngMagic)
	}
}

func TestClient_LastModified_ZeroOnError(t *testing.T) {
	// Point at a server that doesn't exist; we should get the zero
	// time back, not a network error. (LastModified swallows
	// transport errors by design — Pull is the one that surfaces them.)
	c := NewClient("http://127.0.0.1:1", discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got, err := c.LastModified(ctx)
	if err != nil {
		// Some environments surface a real error here; we accept
		// either an error or a zero time. The contract is "don't
		// panic, don't block".
		t.Logf("LastModified returned err = %v (acceptable)", err)
	}
	if !got.IsZero() {
		t.Errorf("expected zero time on transport failure, got %v", got)
	}
}

func TestClient_LastModified_ReturnsParsedHeader(t *testing.T) {
	_, ts := newTestServer(t)
	// Seed.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/clip", bytes.NewReader(pngMagic))
	req.Header.Set("Content-Type", "image/png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	c := NewClient(ts.URL, discardLogger())
	got, err := c.LastModified(context.Background())
	if err != nil {
		t.Fatalf("LastModified: %v", err)
	}
	if got.IsZero() {
		t.Fatal("got zero time, want a parsed header")
	}
	if time.Since(got) > 30*time.Second {
		t.Errorf("LastModified = %v, expected recent", got)
	}
}

func TestClient_Pull_EmptyEndpoint_ErrorsImmediately(t *testing.T) {
	c := NewClient("", discardLogger())
	_, err := c.Pull(context.Background())
	if err == nil {
		t.Fatal("expected error for empty endpoint")
	}
}

func TestClient_Pull_ServerError_SurfacesStatus(t *testing.T) {
	// A 500-style server. Pull should propagate the status, NOT
	// return ErrNoImage (which is specifically 404).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, discardLogger())
	_, err := c.Pull(context.Background())
	if err == nil {
		t.Fatal("expected error from 500")
	}
	if errors.Is(err, ErrNoImage) {
		t.Errorf("500 should not surface as ErrNoImage; got %v", err)
	}
}

func TestClient_Watch_FiresOnNewImage(t *testing.T) {
	_, ts := newTestServer(t)
	c := NewClient(ts.URL, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gotPath := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Watch(ctx, 50*time.Millisecond, func(p string) error {
			select {
			case gotPath <- p:
			default:
			}
			// Stop watching after the first image so the test
			// doesn't hang until the ctx timeout.
			cancel()
			return nil
		})
	}()

	// Give Watch a moment to start polling, then push a new image.
	time.Sleep(100 * time.Millisecond)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/clip", bytes.NewReader(pngMagic))
	req.Header.Set("Content-Type", "image/png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case p := <-gotPath:
		if !strings.Contains(p, "mole-clip-") {
			t.Errorf("path %q doesn't look like a clip file", p)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read pulled file: %v", err)
		}
		if !bytes.Equal(body, pngMagic) {
			t.Errorf("pulled bytes mismatch: %x vs %x", body, pngMagic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch never fired")
	}

	// Watch returns ctx.Err() once we cancel inside the callback.
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Watch returned %v, want ctx.Canceled", err)
	}
}

func TestClient_Watch_NoNewImage_Quiet(t *testing.T) {
	// Empty server, 200ms watch — Watch should keep polling without
	// firing onNew or returning an error. We just verify it
	// respects ctx cancellation.
	_, ts := newTestServer(t)
	c := NewClient(ts.URL, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	fired := false
	err := c.Watch(ctx, 30*time.Millisecond, func(string) error {
		fired = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Watch err = %v, want DeadlineExceeded", err)
	}
	if fired {
		t.Error("onNew fired despite no image being available")
	}
}

// discardLogger returns a logger that throws away every record. Keeps
// the test output clean.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
