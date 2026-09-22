//go:build darwin

package clip

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWatcherPermanentAndTransientResponses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantCalls int64
	}{
		{"too large", http.StatusRequestEntityTooLarge, 1},
		{"unsupported type", http.StatusUnsupportedMediaType, 1},
		{"server error", http.StatusInternalServerError, 2},
		{"request timeout", http.StatusRequestTimeout, 2},
		{"rate limit", http.StatusTooManyRequests, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			status := tc.status
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			}))
			defer server.Close()
			watcher := NewWatcher(discardLogger())
			watcher.readPNG = func(context.Context) ([]byte, error) { return []byte("same image"), nil }
			if err := watcher.tick(context.Background(), server.URL); err == nil {
				t.Fatal("first rejected tick succeeded")
			}
			status = http.StatusNoContent
			if err := watcher.tick(context.Background(), server.URL); err != nil {
				t.Fatal(err)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("PUT calls = %d, want %d", got, tc.wantCalls)
			}
			if tc.wantCalls == 2 {
				if err := watcher.tick(context.Background(), server.URL); err != nil {
					t.Fatal(err)
				}
				if got := calls.Load(); got != 2 {
					t.Fatalf("successful image was sent again: %d calls", got)
				}
			}
		})
	}
}
