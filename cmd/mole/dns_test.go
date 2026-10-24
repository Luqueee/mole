package main

import (
	"encoding/xml"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDNSUnitRoutesOnlyDomainsOnDedicatedLink(t *testing.T) {
	u := dnsUnit([]string{"kena.lan", "corp.example"})
	for _, want := range []string{
		"resolvectl domain mole0 ~kena.lan ~corp.example",
		"resolvectl default-route mole0 no",
		"resolvectl dns mole0 198.18.77.1:5300",
		"ip addr add 198.18.77.1/32 scope global dev mole0",
		"nft -f /etc/mole-dns.nft",
		"nft delete table ip mole; ip link del mole0",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit missing %q", want)
		}
	}
	if strings.Contains(u, " lo ") {
		t.Error("unit must not configure the loopback link")
	}
}

func TestDNSNftRulesRedirectOnlyVirtualIPsToShiftedPorts(t *testing.T) {
	r := dnsNftRules([]int{80, 443}, 7080)
	for _, want := range []string{
		"ip daddr 127.77.0.0/16 tcp dport 80 dnat to :20080",
		"ip daddr 127.77.0.0/16 tcp dport 443 dnat to :20443",
		"ip daddr 127.77.0.1 tcp dport 80 dnat to 127.0.0.1:7080",
		"type nat hook output",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("rules missing %q:\n%s", want, r)
		}
	}
}

// needNetns skips unless user+net namespaces with dummy links and nft work.
func needNetns(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ip", "nft", "unshare"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s", bin)
		}
	}
	if err := exec.Command("unshare", "-rn", "sh", "-c", "ip link add probe0 type dummy && nft add table ip probe").Run(); err != nil {
		t.Skipf("no user+net namespace with dummy links and nft: %v", err)
	}
}

// runStartScript executes dnsStartScript in a throwaway user+net namespace
// with a fake resolvectl that fails on the given subcommand ("" = never) and
// the given nft file ("" = valid rules). extra runs after the script.
func runStartScript(t *testing.T, failOn, nftFile, extra string) (rc int, out string) {
	t.Helper()
	dir := t.TempDir()
	fake := "#!/bin/sh\nexit 0\n"
	if failOn != "" {
		fake = "#!/bin/sh\n[ \"$1\" = \"" + failOn + "\" ] && exit 1\nexit 0\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "resolvectl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if nftFile == "" {
		nftFile = filepath.Join(dir, "mole.nft")
		if err := os.WriteFile(nftFile, []byte(dnsNftRules([]int{80}, 7080)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "ip link set lo up; PATH=" + dir + ":$PATH; " + dnsStartScript([]string{"~kena.lan"}, nftFile) +
		"; rc=$?; ip link show mole0 >/dev/null 2>&1 && echo LINK; nft list table ip mole >/dev/null 2>&1 && echo NFT; " + extra + "exit $rc"
	b, err := exec.Command("unshare", "-rn", "sh", "-c", script).CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Skipf("cannot run unshare: %v", err)
	}
	return rc, string(b)
}

// A failure after the link is created must roll the link back; success keeps
// link and nft table.
func TestDNSStartScriptRollsBackOnPartialFailure(t *testing.T) {
	needNetns(t)
	for _, step := range []string{"dns", "domain", "default-route"} {
		rc, out := runStartScript(t, step, "", "")
		if rc == 0 || strings.Contains(out, "LINK") || strings.Contains(out, "NFT") {
			t.Errorf("resolvectl %s failing: rc=%d out=%q, want failure and nothing left", step, rc, out)
		}
	}
	if rc, out := runStartScript(t, "", "/nonexistent/mole.nft", ""); rc == 0 || strings.Contains(out, "LINK") {
		t.Errorf("bad nft file: rc=%d out=%q, want failure and link rolled back", rc, out)
	}
	rc, out := runStartScript(t, "", "", "")
	if rc != 0 || !strings.Contains(out, "LINK") || !strings.Contains(out, "NFT") {
		t.Errorf("success: rc=%d out=%q, want 0 with link and nft table", rc, out)
	}
}

// The reason for the nft redirect: a server owning 0.0.0.0:80 must not
// hijack the virtual IP's port 80; mole's shifted listener gets it.
func TestDNSNftRedirectBeatsWildcardListener(t *testing.T) {
	needNetns(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	py := `python3 - <<'E'
import socket, threading
w = socket.socket(); w.bind(("0.0.0.0", 80)); w.listen(5)
m = socket.socket(); m.bind(("127.77.0.2", 20080)); m.listen(5)
def srv(s, msg):
    c, _ = s.accept(); c.send(msg); c.close()
threading.Thread(target=srv, args=(w, b"WILDCARD"), daemon=True).start()
threading.Thread(target=srv, args=(m, b"MOLE"), daemon=True).start()
c = socket.create_connection(("127.77.0.2", 80), timeout=3)
print("GOT", c.recv(20).decode())
E
`
	rc, out := runStartScript(t, "", "", py)
	if rc != 0 || !strings.Contains(out, "GOT MOLE") {
		t.Fatalf("rc=%d out=%q, want GOT MOLE", rc, out)
	}
}

func TestMacResolverFilePointsAtLoopbackStub(t *testing.T) {
	got := macResolverFile()
	if !strings.HasPrefix(got, macResolverMarker+"\n") || !strings.Contains(got, "nameserver 127.0.0.1\nport 5300\n") {
		t.Errorf("resolver file:\n%s", got)
	}
}

func TestMacPfRedirectsOnlyTheRouteIP(t *testing.T) {
	got := macPfRules(7080)
	want := "rdr pass on lo0 inet proto tcp from any to 127.77.0.1 port 80 -> 127.0.0.1 port 7080\n"
	if got != want {
		t.Errorf("rules = %q, want %q", got, want)
	}
	if strings.Contains(got, "to any") || strings.Contains(got, "to 127.0.0.1 ") {
		t.Error("the redirect must not capture localhost:80")
	}
}

func TestMacStartScriptAndPlist(t *testing.T) {
	s := macStartScript()
	for _, want := range []string{
		"ifconfig lo0 alias 127.77.0.1 netmask 255.255.255.255",
		"pfctl -a com.apple/mole -f /etc/pf.anchors/mole",
		"pfctl -E",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q: %s", want, s)
		}
	}
	p := macPlist()
	for _, want := range []string{"<string>dev.mole.dns</string>", "<key>RunAtLoad</key>", html.EscapeString(s)} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q", want)
		}
	}
	// '&&' in the script must be escaped: launchd rejects a malformed plist.
	dec := xml.NewDecoder(strings.NewReader(p))
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("plist is not well-formed XML: %v", err)
		}
	}
}

func TestMacPlistPassesPlutil(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("no plutil")
	}
	path := filepath.Join(t.TempDir(), "dev.mole.dns.plist")
	if err := os.WriteFile(path, []byte(macPlist()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v\n%s", err, out)
	}
}
