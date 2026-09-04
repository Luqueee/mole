package config

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg == nil {
		t.Fatal("Default() returned nil")
	}
	if cfg.AdminAddr != "" {
		t.Errorf("AdminAddr = %q, want empty (default is empty; user opts in via YAML)", cfg.AdminAddr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.SSHPort != 22 {
		t.Errorf("SSHPort = %d, want 22", cfg.SSHPort)
	}
	if cfg.AutoDiscover {
		t.Error("AutoDiscover should default to false")
	}
	if len(cfg.DiscoverPorts) == 0 {
		t.Error("DiscoverPorts should be populated by default")
	}
	if len(cfg.Ports) != 0 {
		t.Errorf("Ports should be empty by default, got %v", cfg.Ports)
	}
	if cfg.Remote != "" {
		t.Errorf("Remote should be empty by default, got %q", cfg.Remote)
	}
}

func TestDefault_DiscoverPortsContainsExpected(t *testing.T) {
	cfg := Default()
	want := map[int]bool{
		3000: true, 5173: true, 8080: true, 9000: true,
	}
	for _, p := range cfg.DiscoverPorts {
		if want[p] {
			delete(want, p)
		}
	}
	if len(want) != 0 {
		t.Errorf("DiscoverPorts missing expected entries: %v", want)
	}
}

func TestClipDefaults(t *testing.T) {
	cfg := Default()
	if cfg.ClipListen != "127.0.0.1:7777" {
		t.Errorf("ClipListen = %q, want 127.0.0.1:7777", cfg.ClipListen)
	}
	if cfg.ClipURL != "" {
		t.Errorf("ClipURL = %q, want empty (no default URL — user must set it)", cfg.ClipURL)
	}
	if cfg.ClipIntervalMs != 0 {
		t.Errorf("ClipIntervalMs = %d, want 0 (let the runClipServe default kick in)", cfg.ClipIntervalMs)
	}
}

func TestLoad_ClipPartialYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	// Only override clip_url — listen should be derived from the endpoint
	// so an existing mole.yaml remains reachable after the safer loopback
	// default was introduced.
	content := "clip_url: http://10.0.0.1:7777\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClipURL != "http://10.0.0.1:7777" {
		t.Errorf("ClipURL = %q, want http://10.0.0.1:7777", cfg.ClipURL)
	}
	if cfg.ClipListen != "10.0.0.1:7777" {
		t.Errorf("ClipListen = %q, want address derived from ClipURL", cfg.ClipListen)
	}
	if cfg.ClipIntervalMs != 0 {
		t.Errorf("ClipIntervalMs = %d, want default 0", cfg.ClipIntervalMs)
	}
}

func TestLoad_ExplicitClipListen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := "clip_url: http://100.64.0.10:7777\nclip_listen: 127.0.0.1:7777\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClipListen != "127.0.0.1:7777" {
		t.Errorf("ClipListen = %q, want explicit loopback interface", cfg.ClipListen)
	}
}

func TestLoad_RejectsMalformedClipURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := "clip_url: http://user:sensitive-value@100.64.0.10:abc\nclip_listen: 127.0.0.1:7777\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "invalid clip URL") {
		t.Fatalf("Load() error = %v, want invalid clip URL", err)
	}
	if strings.Contains(err.Error(), "sensitive-value") || strings.Contains(err.Error(), "100.64.0.10") {
		t.Fatalf("Load() exposed URL details in error: %v", err)
	}
}

func TestLoad_RejectsClipURLPortRange(t *testing.T) {
	for name, port := range map[string]string{
		"zero":          "0",
		"above maximum": "65536",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "cfg.yaml")
			content := "clip_url: http://100.64.0.10:" + port + "\n"
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "port must be between 1 and 65535") {
				t.Fatalf("Load() error = %v, want port range validation", err)
			}
		})
	}
}

func TestLoad_EmptyPath(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") returned error: %v", err)
	}
	def := Default()
	if cfg.AdminAddr != def.AdminAddr {
		t.Errorf("AdminAddr = %q, want %q", cfg.AdminAddr, def.AdminAddr)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	cfg, err := Load("/does/not/exist/mole.yaml")
	if err != nil {
		t.Fatalf("Load of missing file should not error, got: %v", err)
	}
	// Should fall back to defaults. AdminAddr now defaults to "" by
	// design (opt-in via YAML), so we use LogLevel — another field
	// that is always populated by Default() — to verify defaults.
	if cfg.LogLevel == "" {
		t.Error("expected defaults to be populated when file is missing")
	}
}

func TestLoad_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := `remote: dev@workstation
ports: [3000, 5173, 8080]
auto_discover: true
admin_addr: ""
log_level: debug
ssh_port: 2222
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Remote != "dev@workstation" {
		t.Errorf("Remote = %q, want %q", cfg.Remote, "dev@workstation")
	}
	wantPorts := []int{3000, 5173, 8080}
	if !reflect.DeepEqual(cfg.Ports, wantPorts) {
		t.Errorf("Ports = %v, want %v", cfg.Ports, wantPorts)
	}
	if !cfg.AutoDiscover {
		t.Error("AutoDiscover = false, want true")
	}
	if cfg.AdminAddr != "" {
		t.Errorf("AdminAddr = %q, want empty", cfg.AdminAddr)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.SSHPort != 2222 {
		t.Errorf("SSHPort = %d, want 2222", cfg.SSHPort)
	}
}

func TestLoad_PartialYAML_MergesWithDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := `remote: dev@workstation
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Remote != "dev@workstation" {
		t.Errorf("Remote = %q, want %q", cfg.Remote, "dev@workstation")
	}
	// Untouched fields keep their defaults.
	if cfg.SSHPort != 22 {
		t.Errorf("SSHPort = %d, want 22 (default)", cfg.SSHPort)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info (default)", cfg.LogLevel)
	}
	if len(cfg.DiscoverPorts) == 0 {
		t.Error("DiscoverPorts should keep defaults when not specified")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("not: valid: yaml: :::"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestLoad_CustomDiscoverPorts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := `
remote: dev@h
discover_ports: [1234, 5678]
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []int{1234, 5678}
	if !reflect.DeepEqual(cfg.DiscoverPorts, want) {
		t.Errorf("DiscoverPorts = %v, want %v", cfg.DiscoverPorts, want)
	}
}

func TestParsePorts(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []int
	}{
		{"empty", "", nil},
		{"single", "3000", []int{3000}},
		{"multi", "3000,5173,8080", []int{3000, 5173, 8080}},
		{"whitespace", " 3000 , 5173 ", []int{3000, 5173}},
		{"empty entries", "3000,,5173,", []int{3000, 5173}},
		{"invalid entries skipped", "3000,abc,5173,xyz", []int{3000, 5173}},
		{"all invalid", "abc,xyz", []int(nil)},
		{"trailing comma only", "3000,", []int{3000}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParsePorts(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParsePorts(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestMergePorts(t *testing.T) {
	tests := []struct {
		name string
		a, b []int
		want []int
	}{
		{
			name: "both empty",
			a:    nil, b: nil,
			want: []int{},
		},
		{
			name: "no overlap, a first",
			a:    []int{1, 2, 3}, b: []int{4, 5, 6},
			want: []int{1, 2, 3, 4, 5, 6},
		},
		{
			name: "full overlap",
			a:    []int{1, 2, 3}, b: []int{1, 2, 3},
			want: []int{1, 2, 3},
		},
		{
			name: "partial overlap",
			a:    []int{1, 2, 3}, b: []int{2, 3, 4},
			want: []int{1, 2, 3, 4},
		},
		{
			name: "duplicates within a",
			a:    []int{1, 1, 2}, b: []int{3},
			want: []int{1, 2, 3},
		},
		{
			name: "empty a",
			a:    nil, b: []int{1, 2},
			want: []int{1, 2},
		},
		{
			name: "empty b",
			a:    []int{1, 2}, b: nil,
			want: []int{1, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergePorts(tt.a, tt.b)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MergePorts(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestMergePorts_OrderPreserved(t *testing.T) {
	// a keeps its order, then new b elements appended in their order.
	got := MergePorts([]int{3000, 5173}, []int{8080, 9000})
	want := []int{3000, 5173, 8080, 9000}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergePorts = %v, want %v", got, want)
	}
}

func TestMergePorts_RealisticDiscoveryFlow(t *testing.T) {
	// Simulates: explicit ports [3000, 5173], auto-discover found [5173, 8080].
	// Result should be [3000, 5173, 8080] with no duplicates.
	got := MergePorts([]int{3000, 5173}, []int{5173, 8080})
	sort.Ints(got)
	want := []int{3000, 5173, 8080}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergePorts = %v, want %v", got, want)
	}
}
