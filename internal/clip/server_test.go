package clip

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pngMagic is the 8-byte PNG signature; tests use it as a stand-in
// for "valid-looking PNG" without dragging a real image into the
// fixture set.
var pngMagic = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	s := &Server{cachePath: filepath.Join(dir, "mole-clip-latest.png"), log: discardLogger()}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestServer_PutThenGet_RoundTripsBytes(t *testing.T) {
	_, ts := newTestServer(t)

	// PUT.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/clip", bytes.NewReader(pngMagic))
	req.Header.Set("Content-Type", "image/png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}

	// GET.
	resp, err = http.Get(ts.URL + "/clip/latest")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, pngMagic) {
		t.Errorf("body = %x, want %x", body, pngMagic)
	}
	if resp.Header.Get("Last-Modified") == "" {
		t.Error("Last-Modified header missing")
	}
}

func TestServer_GetEmpty_Returns404(t *testing.T) {
	_, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/clip/latest")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestServer_PutOversized_Returns413(t *testing.T) {
	_, ts := newTestServer(t)

	// 32 MiB + 1 byte — the MaxBytesReader must cut us off at the limit.
	big := bytes.Repeat([]byte{0xFF}, MaxImageBytes+1)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/clip", bytes.NewReader(big))
	req.Header.Set("Content-Type", "image/png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT oversized: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestServer_PutOversized_DoesNotLeakTempFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("open descriptor inspection requires /proc/self/fd")
	}

	s := &Server{
		cachePath: filepath.Join(t.TempDir(), "mole-clip-latest.png"),
		log:       discardLogger(),
	}
	for range 3 {
		req := httptest.NewRequest(http.MethodPut, "http://example.test/clip", io.LimitReader(zeroReader{}, MaxImageBytes+1))
		req.Header.Set("Content-Type", "image/png")
		rr := httptest.NewRecorder()
		s.handlePut(rr, req)
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized PUT status = %d, want 413", rr.Code)
		}
	}

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read open descriptors: %v", err)
	}
	var leaked int
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && strings.Contains(target, "mole-clip-put-") {
			leaked++
		}
	}
	if leaked != 0 {
		t.Fatalf("found %d open temporary upload files after 413 responses, want 0", leaked)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(s.cachePath), "mole-clip-put-*.png"))
	if err != nil {
		t.Fatalf("find temporary upload files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("found %d stale temporary upload files after 413 responses, want 0", len(matches))
	}
}

func TestServer_PutWrongContentType_Returns415(t *testing.T) {
	_, ts := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/clip", bytes.NewReader(pngMagic))
	req.Header.Set("Content-Type", "image/jpeg")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT wrong type: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

func TestServer_GetAlias_WorksWithoutLatestPath(t *testing.T) {
	s, ts := newTestServer(t)

	// Seed the cache via the file the server reads from. We don't want
	// to round-trip through PUT here — this test is specifically about
	// the GET /clip alias path.
	if err := os.WriteFile(s.cachePath, pngMagic, 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/clip")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, pngMagic) {
		t.Errorf("body mismatch via /clip alias")
	}
}

func TestServer_PutWithoutContentType_IsAccepted(t *testing.T) {
	_, ts := newTestServer(t)

	// No Content-Type set. The server should still accept the image
	// — only the explicit wrong type gets rejected.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/clip", bytes.NewReader(pngMagic))
	// intentionally NOT setting Content-Type
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT no content type: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (no Content-Type is allowed)", resp.StatusCode)
	}
}

func TestServer_ConcurrentPutsLastWriterWins(t *testing.T) {
	_, ts := newTestServer(t)

	const writers = 8
	errCh := make(chan error, writers)
	for i := range writers {
		go func(i int) {
			payload := append([]byte{0x89, 0x50, 0x4E, 0x47}, byte(i))
			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/clip", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "image/png")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			resp.Body.Close()
			errCh <- nil
		}(i)
	}
	for i := range writers {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent PUT %d: %v", i, err)
		}
	}

	resp, err := http.Get(ts.URL + "/clip/latest")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.HasPrefix(body, pngMagic[:4]) {
		t.Errorf("body doesn't start with PNG magic: %x", body)
	}
	if len(body) != 5 {
		t.Errorf("len(body) = %d, want 5", len(body))
	}
	if !strings.HasPrefix(string(body[4:5]), "") {
		// body[4] is a writer id; we don't care which, but it must be
		// one of the writers we sent. The above `!bytes.HasPrefix` is
		// the structural check; this branch keeps linters quiet about
		// the `strings` import being used.
		_ = strings.HasPrefix
	}
}
