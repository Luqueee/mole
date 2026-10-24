package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Luqueee/mole/internal/proxy"
)

// Each event says which way it went (pull -L / push -R) and which port it
// belongs to, and that survives the trip through GET /events.
func TestEventsCarryDirectionAndPort(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(io.Discard, c) }()
		}
	}()

	stats := NewStats()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		hooks proxy.Hooks
		port  int
		want  string
	}{
		{stats.Hooks(proxy.DirPull).WithPort(3000), 3000, "pull"},
		{stats.Hooks(proxy.DirPush).WithPort(8080), 8080, "push"},
		{stats.Hooks("").WithPort(9), 9, "pull"}, // unset means pull
	} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = proxy.Serve(ln, &net.Dialer{}, backend.Addr().String(), tc.hooks, log) }()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("hi"))
		_ = c.Close()
		// Not closed here: if Serve has not accepted the connection yet,
		// closing the listener would drop it and its event would never come.
		t.Cleanup(func() { ln.Close() })
	}

	var got []Event
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		rec := httptest.NewRecorder()
		New(stats, nil).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/events", nil))
		var body struct{ Events []Event }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if got = body.Events; len(got) == 3 {
			break
		}
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %+v", got)
	}
	byPort := map[int]string{}
	for _, e := range got {
		byPort[e.Port] = e.Dir
	}
	if byPort[3000] != "pull" || byPort[8080] != "push" || byPort[9] != "pull" {
		t.Fatalf("direction/port mismatch: %v", byPort)
	}
}
