package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRouteTarget(t *testing.T) {
	for in, want := range map[string]string{
		"3000":                   "127.0.0.1:3000",
		" 3000 ":                 "127.0.0.1:3000",
		"10.0.0.5:8080":          "10.0.0.5:8080",
		"db:5432":                "db:5432",
		"@dockers:3000":          "@dockers/127.0.0.1:3000",
		"@dockers/3000":          "@dockers/127.0.0.1:3000",
		"@dockers/db:5432":       "@dockers/db:5432",
		"@dockers:10.0.0.9:8080": "@dockers/10.0.0.9:8080",
		"@a_b-1:80":              "@a_b-1/127.0.0.1:80",
	} {
		got, err := NormalizeRouteTarget(in)
		if err != nil || got != want {
			t.Errorf("NormalizeRouteTarget(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "65536", ":80", "a b:80", "host:", "host:x", "a/b:80", "@", "@dockers", "@:3000", "@Docker:3000", "@dockers:", "@dockers/@x:80", "https://", "https+insecure://", "https+insecure://:80", "https+insecure:/x", "ftp://x", "admin.corp:", "a:b:c", "https://admin.corp/path"} {
		if got, err := NormalizeRouteTarget(bad); err == nil {
			t.Errorf("NormalizeRouteTarget(%q) = %q, want an error", bad, got)
		}
	}
}

func TestValidateRouteName(t *testing.T) {
	for _, ok := range []string{"a", "api", "my-app", "app2"} {
		if err := ValidateRouteName(ok); err != nil {
			t.Errorf("ValidateRouteName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "A", "a.b", "a_b", "a b", strings.Repeat("x", 64)} {
		if ValidateRouteName(bad) == nil {
			t.Errorf("ValidateRouteName(%q) accepted", bad)
		}
	}
}

func TestDomainAdvice(t *testing.T) {
	for d, warn := range map[string]bool{
		"mole.lan": false, "x.test": false, "mole.localhost": false, "home.arpa": false,
		"mole.local": true, "mole.dev": true, "foo.app": true, "example.com": true,
	} {
		if got := DomainAdvice(d); (got != "") != warn {
			t.Errorf("DomainAdvice(%q) = %q, warn=%v", d, got, warn)
		}
	}
}

func TestRoutesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mole.yaml")
	if err := os.WriteFile(path, []byte("remote: devlabs\ndomains: [mole.lan]\nroutes:\n  a: 3000\n  api: \"10.0.0.5:8080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes["a"] != "127.0.0.1:3000" || cfg.Routes["api"] != "10.0.0.5:8080" {
		t.Fatalf("Routes = %v (a bare port must normalise to the remote loopback)", cfg.Routes)
	}
	if cfg.RoutePortOrDefault() != DefaultRoutePort {
		t.Errorf("RoutePortOrDefault = %d", cfg.RoutePortOrDefault())
	}

	delete(cfg.Routes, "a")
	if err := Save(path, cfg, "routes"); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Routes["a"]; ok || again.Routes["api"] != "10.0.0.5:8080" || again.Remote != "devlabs" {
		t.Fatalf("after save: %+v", again)
	}

	delete(again.Routes, "api")
	if err := Save(path, again, "routes"); err != nil {
		t.Fatal(err)
	}
	last, err := Load(path)
	if err != nil || len(last.Routes) != 0 {
		t.Fatalf("emptying routes: %v, %v", last, err)
	}
}

func TestRoutesRejectedWhenInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"bad name":   "routes:\n  A.b: 3000\n",
		"bad target": "routes:\n  a: \"two words\"\n",
		"low port":   "route_port: 80\n",
	} {
		path := filepath.Join(t.TempDir(), "mole.yaml")
		if err := os.WriteFile(path, []byte("remote: devlabs\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load accepted it", name)
		}
	}
}

func TestRouteNode(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:3000":          "",
		"10.0.0.5:8080":           "",
		"@dockers/127.0.0.1:3000": "dockers",
		"@a-b/db:5432":            "a-b",
	} {
		if got := RouteNode(in); got != want {
			t.Errorf("RouteNode(%q) = %q, want %q", in, got, want)
		}
	}
}
