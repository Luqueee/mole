package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/transfer"
)

// mole status finds the admin API where mole up listens: admin_addr in the
// config, not a hard-coded 127.0.0.1:9999.
func TestStatusReadsAdminAddrFromConfig(t *testing.T) {
	api := httptest.NewServer(admin.New(admin.NewStats(), map[string]any{"remote": "devlabs"}).Handler())
	defer api.Close()
	dir := t.TempDir()
	path := writeConfig(t, dir, "remote: devlabs\nadmin_addr: "+strings.TrimPrefix(api.URL, "http://")+"\n")

	var code int
	out, errOut := capture(t, func() { code = runStatus([]string{"-config", path}) })
	if code != 0 || !strings.Contains(out, `"remote":"devlabs"`) {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}

	// -admin wins over the config.
	other := writeConfig(t, t.TempDir(), "remote: devlabs\nadmin_addr: 127.0.0.1:1\n")
	out, errOut = capture(t, func() {
		code = runStatus([]string{"-config", other, "-admin", strings.TrimPrefix(api.URL, "http://")})
	})
	if code != 0 || !strings.Contains(out, `"stats"`) {
		t.Fatalf("-admin: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestStatusAdminDisabled(t *testing.T) {
	path := writeConfig(t, t.TempDir(), "remote: devlabs\n")
	var code int
	_, errOut := capture(t, func() { code = runStatus([]string{"-config", path}) })
	if code != 1 || !strings.Contains(errOut, "disabled") || !strings.Contains(errOut, path) || !strings.Contains(errOut, "admin_addr: 127.0.0.1:9999") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

// Every request the CLI makes with a body carries the Content-Type the admin
// API requires; otherwise the daemon answers 415.
func TestAdminClientsSendContentType(t *testing.T) {
	got := map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		got[r.Method+" "+r.URL.Path] = r.Header.Get("Content-Type")
		switch r.URL.Path {
		case "/transfer/send":
			_, _ = w.Write([]byte(`{"name":"a.txt","saved":"a.txt"}`))
		case "/ports/discover", "/ports/push", "/routes":
			w.WriteHeader(http.StatusCreated)
		}
	})
	api := httptest.NewServer(mux)
	defer api.Close()
	addr := strings.TrimPrefix(api.URL, "http://")

	if err := adminPostPort(addr, 3000); err != nil {
		t.Fatal(err)
	}
	if err := adminDo(http.MethodPost, addr, "/ports/push", `{"local":3000}`, http.StatusCreated); err != nil {
		t.Fatal(err)
	}
	if err := adminJSON(http.MethodPost, addr, "/transfer/pending/1", map[string]bool{"accept": true}, nil); err != nil {
		t.Fatal(err)
	}
	f, err := transfer.FileFromPath(writeTemp(t, "a.txt", []byte("hello")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uploadToDaemon(t.Context(), addr, f, ""); err != nil {
		t.Fatal(err)
	}

	for req, want := range map[string]string{
		"POST /ports/discover":     admin.ContentTypeJSON,
		"POST /ports/push":         admin.ContentTypeJSON,
		"POST /transfer/pending/1": admin.ContentTypeJSON,
		"PUT /transfer/send":       admin.ContentTypeFile,
	} {
		if got[req] != want {
			t.Errorf("%s: Content-Type %q, want %q", req, got[req], want)
		}
	}
}
