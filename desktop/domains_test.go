package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root, path, body string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDNSStateDarwin(t *testing.T) {
	root := t.TempDir()
	if st := dnsStateAt("darwin", root, nil, 7080); st.Installed || !st.Supported {
		t.Errorf("no domain: %+v", st)
	}
	if st := dnsStateAt("darwin", root, []string{"mole.lan"}, 7080); st.Installed {
		t.Errorf("empty system reported installed: %+v", st)
	}

	write(t, root, "etc/resolver/mole.lan", macResolverMarker+"\nnameserver 127.0.0.1\nport 5300\n")
	write(t, root, "etc/pf.anchors/mole", "rdr pass on lo0 inet proto tcp from any to 127.77.0.1 port 80 -> 127.0.0.1 port 7080\n")
	write(t, root, "Library/LaunchDaemons/dev.mole.dns.plist", "launch daemon")
	if st := dnsStateAt("darwin", root, []string{"mole.lan"}, 7080); !st.Installed {
		t.Errorf("installed: %+v", st)
	}
	if st := dnsStateAt("darwin", root, []string{"mole.lan"}, 7480); st.Installed {
		t.Errorf("stale redirect reported installed: %+v", st)
	}
	// a new domain is not routed until the install is re-run
	if st := dnsStateAt("darwin", root, []string{"other.lan"}, 7080); st.Installed {
		t.Errorf("unrouted domain reported installed: %+v", st)
	}
	// a resolver file that is not mole's does not count
	write(t, root, "etc/resolver/foreign.lan", "nameserver 10.0.0.1\n")
	if st := dnsStateAt("darwin", root, []string{"foreign.lan"}, 7080); st.Installed {
		t.Errorf("foreign resolver counted as installed: %+v", st)
	}
}

func TestDNSStateLinuxAndUnsupported(t *testing.T) {
	root := t.TempDir()
	if st := dnsStateAt("linux", root, []string{"mole.lan"}, 7080); st.Installed || !st.Supported {
		t.Errorf("linux empty: %+v", st)
	}
	write(t, root, "etc/mole-dns.nft", "ip daddr 127.77.0.1 tcp dport 80 dnat to 127.0.0.1:7080\n")
	write(t, root, "etc/systemd/system/mole-dns.service", "resolvectl domain mole0 ~mole.lan2\n")
	if st := dnsStateAt("linux", root, []string{"mole.lan"}, 7080); st.Installed {
		t.Errorf("prefix domain incorrectly matched: %+v", st)
	}
	write(t, root, "etc/systemd/system/mole-dns.service", "resolvectl domain mole0 ~mole.lan\n")
	if st := dnsStateAt("linux", root, []string{"mole.lan"}, 7080); !st.Installed {
		t.Errorf("linux installed: %+v", st)
	}
	if st := dnsStateAt("linux", root, []string{"other.lan"}, 7080); st.Installed {
		t.Errorf("stale domain configuration reported installed: %+v", st)
	}
	if st := dnsStateAt("linux", root, []string{"mole.lan"}, 7480); st.Installed {
		t.Errorf("linux stale redirect reported installed: %+v", st)
	}
	if st := dnsStateAt("windows", root, []string{"mole.lan"}, 7080); st.Supported || st.Installed {
		t.Errorf("windows: %+v", st)
	}
}

func TestRouteURL(t *testing.T) {
	if got := routeURL("a.mole.lan", true, 7080); got != "http://a.mole.lan" {
		t.Errorf("installed: %s", got)
	}
	if got := routeURL("a.mole.lan", false, 7080); got != "" {
		t.Errorf("custom domain without DNS must not produce a broken URL: %q", got)
	}
	if got := routeURL("a.dev.localhost", false, 7080); got != "http://a.dev.localhost:7080" {
		t.Errorf(".localhost rootless URL: %s", got)
	}
	if got := routeURL("a.dev.localhost", true, 7080); got != "http://a.dev.localhost:7080" {
		t.Errorf(".localhost must bypass system DNS even when it is installed: %s", got)
	}
}

func TestShellJoinQuotesEveryArgument(t *testing.T) {
	got := shellJoin([]string{"/Users/o'brien/mole", "dns", "install", "-config", "/tmp/a b/$HOME;rm -rf x"})
	want := `'/Users/o'\''brien/mole' 'dns' 'install' '-config' '/tmp/a b/$HOME;rm -rf x'`
	if got != want {
		t.Errorf("shellJoin = %s\nwant       %s", got, want)
	}
}

func TestAppleScriptStringEscapes(t *testing.T) {
	if got := appleScriptString(`a "b" \c`); got != `"a \"b\" \\c"` {
		t.Errorf("appleScriptString = %s", got)
	}
}

func TestPrivilegedCommand(t *testing.T) {
	cmd, err := privilegedCommand(context.Background(), "darwin", []string{"/bin/mole", "dns", "install", "-config", "/p/c.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.Args) != 3 || cmd.Args[0] != "osascript" || cmd.Args[1] != "-e" ||
		cmd.Args[2] != `do shell script "'/bin/mole' 'dns' 'install' '-config' '/p/c.yaml'" with administrator privileges` {
		t.Errorf("darwin args = %q", cmd.Args)
	}
	if _, err := privilegedCommand(context.Background(), "windows", nil); err == nil {
		t.Error("windows must report unsupported")
	}
}
