package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mole.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoadPushDefaults(t *testing.T) {
	cfg, err := loadYAML(t, "remote: a@b\npush:\n  - local: 3000\n  - local: 4000\n    remote: 8080\n    bind: 0.0.0.0\n")
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Push[0], cfg.Push[1]
	if a.RemotePort() != 3000 || a.BindAddr() != "127.0.0.1" {
		t.Errorf("defaults: %+v", a)
	}
	if b.RemotePort() != 8080 || b.BindAddr() != "0.0.0.0" {
		t.Errorf("explicit: %+v", b)
	}
}

func TestLoadPushRejectsInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"range":   "push:\n  - local: 70000\n",
		"remote":  "push:\n  - local: 3000\n    remote: -1\n",
		"dup":     "push:\n  - local: 3000\n  - local: 3000\n    remote: 1\n",
		"unknown": "push:\n  - local: 3000\n    nope: 1\n",
	} {
		if _, err := loadYAML(t, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSavePushPreservesRestAndRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mole.yaml")
	if err := os.WriteFile(p, []byte("# keep me\nremote: a@b\nports: [1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := Load(p)
	cfg.Push = []PushRule{{Local: 3000, Remote: 8080}}
	if err := Save(p, cfg, "push"); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil || len(got.Push) != 1 || got.Push[0].RemotePort() != 8080 || got.Remote != "a@b" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if data, _ := os.ReadFile(p); !strings.Contains(string(data), "# keep me") {
		t.Error("comment lost")
	}
}

func TestLoadDomainsValidation(t *testing.T) {
	cfg, err := loadYAML(t, "domains: [kena.lan]\ndomain_ports: [8443]\n")
	if err != nil || cfg.Domains[0] != "kena.lan" || cfg.DomainPortsOrDefault()[0] != 8443 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if got := (&Config{}).DomainPortsOrDefault(); len(got) != 2 || got[0] != 80 || got[1] != 443 {
		t.Errorf("default ports = %v", got)
	}
	for _, bad := range []string{"domains: ['a b']", "domains: ['x;rm']", "domains: ['.lan']", "domains: ['A.lan']", "domain_ports: [0]"} {
		if _, err := loadYAML(t, bad+"\n"); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
