package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useDaemon points the app's config at a fake daemon served by h.
func useDaemon(t *testing.T, h http.Handler) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Chdir(t.TempDir()) // no ./mole.yaml to shadow the global config
	cfgDir := filepath.Join(dir, "mole")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "admin_addr: " + strings.TrimPrefix(ts.URL, "http://") + "\ntransfer:\n  inbox: " + filepath.Join(dir, "inbox") + "\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGetTransfers(t *testing.T) {
	useDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transfer/log" || r.URL.Query().Get("since") != "5" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"records":null,"active":[{"id":2,"dir":"send","name":"a","size":10,"bytes":4}],"pending":[{"id":7,"peer":"devlabs","files":[{"name":"x","size":1}],"total":1}],"last":9}`)
	}))
	v := NewMoleService().GetTransfers(5)
	if !v.Supported || v.Last != 9 || len(v.Active) != 1 || v.Active[0].Bytes != 4 || len(v.Pending) != 1 || v.Pending[0].ID != 7 {
		t.Fatalf("view: %+v", v)
	}
	if v.Records == nil {
		t.Fatal("records must be an empty list, not null, for the frontend")
	}
}

func TestGetTransfersWithOldDaemon(t *testing.T) {
	useDaemon(t, http.NotFoundHandler()) // a daemon without /transfer
	v := NewMoleService().GetTransfers(0)
	if v.Supported || v.Records == nil || v.Active == nil || v.Pending == nil {
		t.Fatalf("view: %+v", v)
	}
}

func TestSendFilesUploadsBytesNotPaths(t *testing.T) {
	type got struct {
		to, body string
		length   int64
		mode     string
	}
	seen := map[string]got{}
	useDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/transfer/send" {
			http.NotFound(w, r)
			return
		}
		name := r.URL.Query().Get("name")
		b, _ := io.ReadAll(r.Body)
		seen[name] = got{to: r.URL.Query().Get("to"), body: string(b), length: r.ContentLength, mode: r.URL.Query().Get("mode")}
		if name == "refused.txt" {
			http.Error(w, "the other side refused: no space", http.StatusUnprocessableEntity)
			return
		}
		io.WriteString(w, `{"name":"`+name+`","saved":"saved-`+name+`","bytes":5,"dir":"/inbox"}`)
	}))
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.txt")
	empty := filepath.Join(dir, "empty.txt")
	refused := filepath.Join(dir, "refused.txt")
	os.WriteFile(ok, []byte("hello"), 0o640)
	os.WriteFile(empty, nil, 0o644)
	os.WriteFile(refused, []byte("x"), 0o644)

	res := NewMoleService().SendFiles([]string{ok, empty, refused, filepath.Join(dir, "missing"), dir}, " docs ")
	if len(res) != 5 {
		t.Fatalf("results: %+v", res)
	}
	if res[0].Error != "" || res[0].Saved != "saved-ok.txt" || res[0].Dir != "/inbox" {
		t.Fatalf("ok: %+v", res[0])
	}
	if g := seen["ok.txt"]; g.body != "hello" || g.length != 5 || g.to != "docs" || g.mode != "416" {
		t.Fatalf("what the daemon received for ok.txt: %+v", g)
	}
	if res[1].Error != "" || res[1].Saved != "saved-empty.txt" {
		t.Fatalf("empty file: %+v", res[1])
	}
	if g := seen["empty.txt"]; g.body != "" || g.length != 0 {
		t.Fatalf("what the daemon received for empty.txt: %+v", g)
	}
	if !strings.Contains(res[2].Error, "no space") {
		t.Fatalf("refusal not reported: %+v", res[2])
	}
	if res[3].Error == "" || !strings.Contains(res[4].Error, "directory") {
		t.Fatalf("missing file / directory: %+v %+v", res[3], res[4])
	}
	if _, sent := seen["missing"]; sent || len(seen) != 3 {
		t.Fatalf("files that cannot be read must not reach the daemon: %v", seen)
	}
}

func TestResolveTransferAndPendingCount(t *testing.T) {
	var path, body string
	useDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/transfer/pending/"):
			b, _ := io.ReadAll(r.Body)
			path, body = r.URL.Path, string(b)
			if strings.HasSuffix(r.URL.Path, "/99") {
				http.Error(w, "no such pending transfer", http.StatusNotFound)
				return
			}
			io.WriteString(w, `{"status":"ok"}`)
		case r.URL.Path == "/transfer/pending":
			io.WriteString(w, `[{"id":1},{"id":2}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	svc := NewMoleService()
	if err := svc.ResolveTransfer(7, true); err != nil || path != "/transfer/pending/7" || !strings.Contains(body, `"accept":true`) {
		t.Fatalf("accept: %v %s %s", err, path, body)
	}
	if err := svc.ResolveTransfer(99, false); err == nil || !strings.Contains(err.Error(), "no such pending") {
		t.Fatalf("unknown id: %v", err)
	}
	if n := svc.pendingCount(); n != 2 {
		t.Fatalf("pendingCount = %d", n)
	}
}

func TestOpenInboxCreatesTheFolder(t *testing.T) {
	useDaemon(t, http.NotFoundHandler())
	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := cfg.Transfer.InboxDir()
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("inbox exists before the test")
	}
	// Starting the file manager is environment-specific; what matters is that
	// the folder exists for it to show.
	_ = NewMoleService().OpenInbox()
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("inbox not created: %v", err)
	}
}
