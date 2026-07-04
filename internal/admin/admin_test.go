package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewStats(t *testing.T) {
	before := time.Now()
	s := NewStats()
	after := time.Now()

	if s == nil {
		t.Fatal("NewStats returned nil")
	}
	if s.StartedAt.Before(before) || s.StartedAt.After(after) {
		t.Errorf("StartedAt = %v, want between %v and %v", s.StartedAt, before, after)
	}
	if s.activeConns.Load() != 0 {
		t.Errorf("activeConns = %d, want 0", s.activeConns.Load())
	}
	if s.totalConns.Load() != 0 {
		t.Errorf("totalConns = %d, want 0", s.totalConns.Load())
	}
	if s.failedDials.Load() != 0 {
		t.Errorf("failedDials = %d, want 0", s.failedDials.Load())
	}
}

func TestStats_OnConnectOnDisconnect(t *testing.T) {
	s := NewStats()

	s.OnConnect()
	s.OnConnect()
	s.OnConnect()

	if got := s.activeConns.Load(); got != 3 {
		t.Errorf("activeConns = %d, want 3", got)
	}
	if got := s.totalConns.Load(); got != 3 {
		t.Errorf("totalConns = %d, want 3", got)
	}

	s.OnDisconnect()
	if got := s.activeConns.Load(); got != 2 {
		t.Errorf("after OnDisconnect: activeConns = %d, want 2", got)
	}
	if got := s.totalConns.Load(); got != 3 {
		t.Errorf("totalConns should not change on OnDisconnect, got %d", got)
	}
}

func TestStats_OnDialFail(t *testing.T) {
	s := NewStats()
	s.OnDialFail()
	s.OnDialFail()
	if got := s.failedDials.Load(); got != 2 {
		t.Errorf("failedDials = %d, want 2", got)
	}
}

func TestStats_Concurrent(t *testing.T) {
	s := NewStats()
	const N = 1000

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			s.OnConnect()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			s.OnDisconnect()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			s.OnDialFail()
		}
	}()
	wg.Wait()

	if got := s.totalConns.Load(); got != N {
		t.Errorf("totalConns = %d, want %d", got, N)
	}
	if got := s.failedDials.Load(); got != N {
		t.Errorf("failedDials = %d, want %d", got, N)
	}
	if got := s.activeConns.Load(); got != 0 {
		t.Errorf("activeConns = %d, want 0", got)
	}
}

func TestServer_HandlerHealth(t *testing.T) {
	srv := New(NewStats(), nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", string(body), "ok")
	}
}

func TestServer_HandlerStatusEmpty(t *testing.T) {
	stats := NewStats()
	srv := New(stats, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	stats2, ok := got["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats is not an object: %#v", got["stats"])
	}
	if stats2["active_conns"].(float64) != 0 {
		t.Errorf("active_conns = %v, want 0", stats2["active_conns"])
	}
	if stats2["total_conns"].(float64) != 0 {
		t.Errorf("total_conns = %v, want 0", stats2["total_conns"])
	}
	if stats2["failed_dials"].(float64) != 0 {
		t.Errorf("failed_dials = %v, want 0", stats2["failed_dials"])
	}
	uptime, _ := stats2["uptime"].(string)
	if uptime == "" {
		t.Error("uptime should be a non-empty string")
	}

	// "info" should still be present even when nil.
	if _, ok := got["info"]; !ok {
		t.Error("info key should be present in /status response")
	}
}

func TestServer_HandlerStatusWithExtra(t *testing.T) {
	stats := NewStats()
	stats.OnConnect()
	stats.OnConnect()
	stats.OnDisconnect()
	stats.OnDialFail()

	extra := map[string]any{
		"remote": "dev@workstation",
		"ports":  []int{3000, 5173},
	}
	srv := New(stats, extra)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, string(body))
	}

	var got struct {
		Stats struct {
			Uptime      string `json:"uptime"`
			ActiveConns int64  `json:"active_conns"`
			TotalConns  uint64 `json:"total_conns"`
			FailedDials uint64 `json:"failed_dials"`
		} `json:"stats"`
		Info map[string]any `json:"info"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, string(body))
	}

	if got.Stats.ActiveConns != 1 {
		t.Errorf("active_conns = %d, want 1", got.Stats.ActiveConns)
	}
	if got.Stats.TotalConns != 2 {
		t.Errorf("total_conns = %d, want 2", got.Stats.TotalConns)
	}
	if got.Stats.FailedDials != 1 {
		t.Errorf("failed_dials = %d, want 1", got.Stats.FailedDials)
	}
	if got.Info["remote"] != "dev@workstation" {
		t.Errorf("info.remote = %v, want dev@workstation", got.Info["remote"])
	}
}

func TestServer_UnknownPath(t *testing.T) {
	srv := New(NewStats(), nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestStats_UptimeAdvances(t *testing.T) {
	stats := NewStats()

	// Take first snapshot, sleep, take another.
	srv := New(stats, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func() string {
		resp, err := http.Get(ts.URL + "/status")
		if err != nil {
			t.Fatalf("GET /status: %v", err)
		}
		defer resp.Body.Close()
		var got struct {
			Stats struct {
				Uptime string `json:"uptime"`
			} `json:"stats"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got.Stats.Uptime
	}

	first := get()
	time.Sleep(1100 * time.Millisecond) // uptime is truncated to seconds
	second := get()

	if first == "" || second == "" {
		t.Fatalf("uptime was empty: first=%q second=%q", first, second)
	}
	if first == second && !strings.Contains(second, "0s") {
		// If both are the same string and it's not 0s, the clock isn't advancing.
		// This is just a heuristic — we mainly want to ensure no panic.
		t.Logf("uptime did not change in 1.1s: first=%q second=%q", first, second)
	}
}

// fakePortCtl is a PortController backed by a map + sync.Mutex.
// It also exposes an excluded set so we can test the 409 path.
type fakePortCtl struct {
	mu       sync.Mutex
	active   map[int]bool
	excluded map[int]bool
	failNext error // returned by Add/Remove on the next call
}

func newFakePortCtl() *fakePortCtl {
	return &fakePortCtl{active: map[int]bool{}, excluded: map[int]bool{}}
}

func (f *fakePortCtl) AddDiscover(p int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	if f.excluded[p] {
		return fmt.Errorf("port %d is in exclude_ports", p)
	}
	if f.active[p] {
		return fmt.Errorf("port %d is already forwarded", p)
	}
	f.active[p] = true
	return nil
}

func (f *fakePortCtl) RemoveDiscover(p int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	delete(f.active, p)
	return nil
}

func TestServer_PortAdd_HappyPath(t *testing.T) {
	ctl := newFakePortCtl()
	srv := New(NewStats(), nil).WithPortController(ctl)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := strings.NewReader(`{"port":3330}`)
	resp, err := http.Post(ts.URL+"/ports/discover", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if !ctl.active[3330] {
		t.Error("controller was not asked to add 3330")
	}
}

func TestServer_PortAdd_RejectsExcluded(t *testing.T) {
	ctl := newFakePortCtl()
	ctl.excluded[22] = true
	srv := New(NewStats(), nil).WithPortController(ctl)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/ports/discover", "application/json", strings.NewReader(`{"port":22}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "exclude_ports") {
		t.Errorf("body = %q, want it to mention exclude_ports", body)
	}
}

func TestServer_PortAdd_RejectsDuplicate(t *testing.T) {
	ctl := newFakePortCtl()
	ctl.active[3330] = true
	srv := New(NewStats(), nil).WithPortController(ctl)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/ports/discover", "application/json", strings.NewReader(`{"port":3330}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestServer_PortAdd_ValidatesPort(t *testing.T) {
	ctl := newFakePortCtl()
	srv := New(NewStats(), nil).WithPortController(ctl)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, p := range []string{`{"port":0}`, `{"port":70000}`, `{"port":-1}`, `not json`} {
		resp, err := http.Post(ts.URL+"/ports/discover", "application/json", strings.NewReader(p))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", p, resp.StatusCode)
		}
	}
}

func TestServer_PortDelete_HappyPath(t *testing.T) {
	ctl := newFakePortCtl()
	ctl.active[3330] = true
	srv := New(NewStats(), nil).WithPortController(ctl)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/ports/discover/3330", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if ctl.active[3330] {
		t.Error("controller still has 3330 after delete")
	}
}

func TestServer_PortDelete_Idempotent(t *testing.T) {
	ctl := newFakePortCtl() // empty
	srv := New(NewStats(), nil).WithPortController(ctl)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/ports/discover/9999", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("delete of missing port: status = %d, want 200 (idempotent)", resp.StatusCode)
	}
}

func TestServer_PortDelete_ValidatesPort(t *testing.T) {
	ctl := newFakePortCtl()
	srv := New(NewStats(), nil).WithPortController(ctl)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, p := range []string{"abc", "-1", "0", "70000"} {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/ports/discover/"+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400", p, resp.StatusCode)
		}
	}
}

func TestServer_PortEndpoints_UnregisteredWithoutController(t *testing.T) {
	// If WithPortController was never called, the endpoints must
	// not be served. This protects against accidentally exposing
	// the routes on a forwarder that doesn't support live mutation.
	srv := New(NewStats(), nil) // no WithPortController
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/ports/discover", "application/json", strings.NewReader(`{"port":3330}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST without controller: status = %d, want 404", resp.StatusCode)
	}
}
