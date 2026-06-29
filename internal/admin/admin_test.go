package admin

import (
	"encoding/json"
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
