package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Luqueee/mole/internal/config"
)

// chdir changes into dir for the duration of the test, restoring
// the original cwd via t.Cleanup. Used to keep ports tests from
// reading or writing the user's real ~/.config/mole/config.yaml.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "mole.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPortsAdd_AppendsAndSorts(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\nauto_discover: true\ndiscover_ports: [5173, 3000]\n")

	if code := runPorts([]string{"add", "3330"}); code != 0 {
		t.Fatalf("add returned %d, want 0", code)
	}
	cfg, err := config.Load(filepath.Join(dir, "mole.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []int{3000, 3330, 5173}
	if !equalInts(cfg.DiscoverPorts, want) {
		t.Errorf("DiscoverPorts = %v, want %v", cfg.DiscoverPorts, want)
	}
}

func TestPortsAdd_Idempotent(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\nauto_discover: true\ndiscover_ports: [3000, 5173]\n")

	// Adding an existing port should be a no-op (exit 0, no error).
	if code := runPorts([]string{"add", "3000"}); code != 0 {
		t.Fatalf("add 3000 (existing) returned %d, want 0", code)
	}
	cfg, err := config.Load(filepath.Join(dir, "mole.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(cfg.DiscoverPorts, []int{3000, 5173}) {
		t.Errorf("DiscoverPorts = %v, want unchanged [3000 5173]", cfg.DiscoverPorts)
	}
}

func TestPortsAdd_RefusesExcludedPort(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\nauto_discover: true\nexclude_ports: [22, 25]\n")
	if code := runPorts([]string{"add", "22"}); code != 1 {
		t.Errorf("add 22 (excluded) returned %d, want 1", code)
	}
	cfg, err := config.Load(filepath.Join(dir, "mole.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// DiscoverPorts will contain the package defaults (config.Load
	// merges them in), but the add should have been rejected
	// regardless. The contract is "22 is NOT in the on-disk list
	// after the failed add" — the list being non-empty from
	// defaults is fine.
	for _, p := range cfg.DiscoverPorts {
		if p == 22 {
			t.Errorf("DiscoverPorts contains 22 after rejected add: %v", cfg.DiscoverPorts)
		}
	}
}
func TestPortsAdd_RejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\n")

	cases := []struct {
		in   string
		want int
	}{
		{"abc", 2},   // not a number
		{"0", 2},     // out of range
		{"70000", 2}, // too big
		{"", 2},      // empty
		// Note: we don't test "-1" because flag.Parse interprets
		// a leading dash as a flag, not a positional. The "-1"
		// case is still rejected by the atoi/Range check in
		// production via the "1-65535" message, but the test
		// can't reach it through fs.Args.
	}
	for _, tc := range cases {
		if got := runPorts([]string{"add", tc.in}); got != tc.want {
			t.Errorf("add %q returned %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPortsAdd_RequiresOneArg(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\n")

	if code := runPorts([]string{"add"}); code != 2 {
		t.Errorf("add (no args) returned %d, want 2", code)
	}
	if code := runPorts([]string{"add", "3330", "4440"}); code != 2 {
		t.Errorf("add (too many args) returned %d, want 2", code)
	}
}

func TestPortsRemove_DropsAndSaves(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\ndiscover_ports: [3000, 3330, 5173]\n")

	if code := runPorts([]string{"remove", "3330"}); code != 0 {
		t.Fatalf("remove returned %d, want 0", code)
	}
	cfg, err := config.Load(filepath.Join(dir, "mole.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(cfg.DiscoverPorts, []int{3000, 5173}) {
		t.Errorf("DiscoverPorts = %v, want [3000 5173]", cfg.DiscoverPorts)
	}
}

func TestPortsRemove_Idempotent(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\ndiscover_ports: [3000]\n")

	// Removing a port that's not there should be a no-op.
	if code := runPorts([]string{"remove", "9999"}); code != 0 {
		t.Errorf("remove 9999 (missing) returned %d, want 0", code)
	}
	cfg, _ := config.Load(filepath.Join(dir, "mole.yaml"))
	if !equalInts(cfg.DiscoverPorts, []int{3000}) {
		t.Errorf("DiscoverPorts = %v, want [3000]", cfg.DiscoverPorts)
	}
}

func TestPortsList_PrintsBothLists(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\ndiscover_ports: [3000, 5173]\nexclude_ports: [22]\n")
	var code int
	out := captureStdout(t, func() {
		code = runPorts([]string{"list"})
	})
	if code != 0 {
		t.Errorf("list returned %d, want 0", code)
	}
	for _, want := range []string{
		"discover_ports:",
		"3000",
		"5173",
		"exclude_ports:",
		"22",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s\n---", want, out)
		}
	}
}

func TestPortsList_NoConfig_PrintsDefaults(t *testing.T) {
	// config.Load() falls back to defaults when the file is missing
	// (that's the same behaviour `mole up` exhibits). list should
	// honour that: an absent config is not an error, it's "show
	// me what the defaults would be". This is a deliberate
	// contract — failing here would be inconsistent with runUp.
	dir := t.TempDir()
	chdir(t, dir)
	var code int
	out := captureStdout(t, func() {
		code = runPorts([]string{"list", "-config", filepath.Join(dir, "nonexistent.yaml")})
	})
	if code != 0 {
		t.Errorf("list with missing config returned %d, want 0 (defaults)", code)
	}
	// Default DiscoverPorts contains 3000.
	if !strings.Contains(out, "3000") {
		t.Errorf("expected defaults to include 3000; got:\n%s", out)
	}
}

func TestPorts_UnknownSubcommand(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if code := runPorts([]string{"banana"}); code != 2 {
		t.Errorf("ports banana returned %d, want 2", code)
	}
}

func TestPorts_NoSubcommand(t *testing.T) {
	if code := runPorts(nil); code != 2 {
		t.Errorf("ports (no args) returned %d, want 2", code)
	}
}

// TestSave_PreservesComments is the reason we use yaml.Node-based
// save instead of yaml.Marshal(&cfg): the user's comments and key
// order must survive a round-trip.
func TestSave_PreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mole.yaml")
	in := `# mole — generated by ` + "`mole init`" + ` on 2026-07-04T11:21:49Z
# See https://github.com/Luqueee/mole for the full reference.

remote: devlabs
auto_discover: true

# Ports never auto-forwarded (system/reserved). Uncomment to
# override the default [22, 25, 53, 111, 631]; [] excludes nothing.
# exclude_ports: [22, 25, 53, 111, 631]

# Admin HTTP API (set to "" to disable).
admin_addr: 127.0.0.1:9999

log_level: info
ssh_port: 22
`
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DiscoverPorts = []int{3330, 3000, 5173}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Comments must survive. The exact spacing may shift slightly
	// (yaml.v3 normalises indentation in some cases), but the
	// comment text itself is what matters.
	for _, must := range []string{
		"# mole — generated",
		"# See https://github.com/Luqueee/mole",
		"# Ports never auto-forwarded",
		"# override the default",
		"# Admin HTTP API",
	} {
		if !strings.Contains(string(out), must) {
			t.Errorf("output missing comment %q\n---\n%s\n---", must, out)
		}
	}
}

// --- helpers ---

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- live path tests ---

// mockAdminServer is a tiny admin API stand-in for testing the
// live-add path. It implements /health (200), POST /ports/discover
// (201), and DELETE /ports/discover/{port} (200), and tracks the
// port set in memory.
type mockAdminServer struct {
	mu     sync.Mutex
	active map[int]bool
}

func newMockAdmin(t *testing.T) *httptest.Server {
	t.Helper()
	m := &mockAdminServer{active: map[int]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /ports/discover", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Port int }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.active[req.Port] {
			http.Error(w, "already", 409)
			return
		}
		m.active[req.Port] = true
		w.WriteHeader(201)
	})
	mux.HandleFunc("DELETE /ports/discover/", func(w http.ResponseWriter, r *http.Request) {
		var p int
		fmt.Sscanf(r.URL.Path, "/ports/discover/%d", &p)
		m.mu.Lock()
		delete(m.active, p)
		m.mu.Unlock()
		w.WriteHeader(200)
	})
	return httptest.NewServer(mux)
}

func TestPortsAdd_LiveNotifiesAdmin(t *testing.T) {
	admin := newMockAdmin(t)
	defer admin.Close()

	dir := t.TempDir()
	chdir(t, dir)
	addr := strings.TrimPrefix(admin.URL, "http://")
	writeConfig(t, dir, "remote: devlabs\ndiscover_ports: [3000]\nadmin_addr: "+addr+"\n")

	var code int
	out := captureStdout(t, func() {
		code = runPorts([]string{"add", "4440"})
	})
	if code != 0 {
		t.Fatalf("add returned %d, want 0", code)
	}
	if !strings.Contains(out, "live:") {
		t.Errorf("expected live message in output, got:\n%s", out)
	}
	cfg, _ := config.Load(filepath.Join(dir, "mole.yaml"))
	if !equalInts(cfg.DiscoverPorts, []int{3000, 4440}) {
		t.Errorf("DiscoverPorts = %v, want [3000 4440]", cfg.DiscoverPorts)
	}
}

func TestPortsAdd_AdminDownFallsBackToYAML(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\ndiscover_ports: [3000]\nadmin_addr: 127.0.0.1:1\n")

	var code int
	out := captureStdout(t, func() {
		code = runPorts([]string{"add", "4440"})
	})
	if code != 0 {
		t.Errorf("add with admin down: code = %d, want 0", code)
	}
	if !strings.Contains(out, "not running") {
		t.Errorf("expected 'not running' note, got:\n%s", out)
	}
	cfg, _ := config.Load(filepath.Join(dir, "mole.yaml"))
	if !equalInts(cfg.DiscoverPorts, []int{3000, 4440}) {
		t.Errorf("YAML should still be updated; got %v", cfg.DiscoverPorts)
	}
}

func TestPortsAdd_NoAdminAddr_OnlyYAML(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	writeConfig(t, dir, "remote: devlabs\ndiscover_ports: [3000]\n") // no admin_addr

	var code int
	out := captureStdout(t, func() {
		code = runPorts([]string{"add", "4440"})
	})
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if strings.Contains(out, "live:") || strings.Contains(out, "not running") {
		t.Errorf("expected no live messaging; got:\n%s", out)
	}
}

func TestPortsRemove_LiveNotifiesAdmin(t *testing.T) {
	admin := newMockAdmin(t)
	defer admin.Close()

	dir := t.TempDir()
	chdir(t, dir)
	addr := strings.TrimPrefix(admin.URL, "http://")
	writeConfig(t, dir, "remote: devlabs\ndiscover_ports: [3000, 4440]\nadmin_addr: "+addr+"\n")

	var code int
	out := captureStdout(t, func() {
		code = runPorts([]string{"remove", "3000"})
	})
	if code != 0 {
		t.Errorf("remove returned %d, want 0", code)
	}
	if !strings.Contains(out, "live:") {
		t.Errorf("expected live message; got:\n%s", out)
	}
}

func TestDaemonLive_DeadAddr(t *testing.T) {
	if daemonLive("127.0.0.1:1") {
		t.Error("daemonLive on a closed port should return false")
	}
}

func TestDaemonLive_LiveAddr(t *testing.T) {
	admin := newMockAdmin(t)
	defer admin.Close()
	addr := strings.TrimPrefix(admin.URL, "http://")
	if !daemonLive(addr) {
		t.Errorf("daemonLive(%q) = false, want true (admin is up)", addr)
	}
}
