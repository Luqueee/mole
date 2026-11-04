package tunnel

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestResolveAlias_ProxyJump(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX ssh executable")
	}

	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	const config = `#!/bin/sh
printf '%s\n' \
  'user devlabs' \
  'hostname 192.168.1.60' \
  'port 22' \
  'identityfile ~/.ssh/id_ed25519' \
  'proxyjump root@[10.250.0.3]'
`
	if err := os.WriteFile(sshPath, []byte(config), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	remote, err := ResolveRemote("devlabs", 22)
	if err != nil {
		t.Fatal(err)
	}
	if remote.User != "devlabs" || remote.Addr != "192.168.1.60:22" {
		t.Fatalf("target = %#v, want devlabs@192.168.1.60:22", remote)
	}
	if len(remote.ProxyJumps) != 1 {
		t.Fatalf("ProxyJumps = %#v, want one hop", remote.ProxyJumps)
	}
	jump := remote.ProxyJumps[0]
	if jump.User != "root" || jump.Addr != "10.250.0.3:22" {
		t.Fatalf("jump = %#v, want root@10.250.0.3:22", jump)
	}
}

func TestResolveProxyJumpsExplicitChainAndNone(t *testing.T) {
	for _, spec := range []string{"", "none", "NoNe"} {
		jumps, err := resolveProxyJumps(spec, 22)
		if err != nil || len(jumps) != 0 {
			t.Fatalf("resolveProxyJumps(%q) = %v, %v", spec, jumps, err)
		}
	}
	jumps, err := resolveProxyJumps("alice@jump:2200, bob@[::1]", 22)
	if err != nil {
		t.Fatal(err)
	}
	if len(jumps) != 2 || jumps[0].User != "alice" || jumps[0].Addr != "jump:2200" || jumps[1].User != "bob" || jumps[1].Addr != "[::1]:22" {
		t.Fatalf("unexpected ProxyJump chain: %#v", jumps)
	}
	if _, err := resolveProxyJumps("@invalid", 22); err == nil {
		t.Fatal("invalid ProxyJump accepted")
	}
}

func TestParseSSHConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	out := []byte(`host dev
user alice
hostname dev.internal
port 2222
identitiesonly yes
identityfile ~/.ssh/work
identityfile /keys/other
identityagent ~/agent.sock
stricthostkeychecking true
hashknownhosts yes
hostkeyalias dev-box
userknownhostsfile ~/.ssh/known_hosts ~/.ssh/known_hosts2
globalknownhostsfile /etc/ssh/ssh_known_hosts /etc/ssh/ssh_known_hosts2
certificatefile ~/.ssh/work-cert.pub
proxycommand ssh -W %h:%p bastion
`)
	r, proxyJump := parseSSHConfig("dev", out, 22)
	if proxyJump != "" {
		t.Fatalf("proxyJump = %q", proxyJump)
	}
	sshDir := filepath.Join(home, ".ssh")
	want := Remote{
		User:                  "alice",
		Addr:                  "dev.internal:2222",
		IdentityFiles:         []string{filepath.Join(sshDir, "work"), "/keys/other"},
		IdentityAgent:         "~/agent.sock", // expanded when the agent is dialed
		IdentitiesOnly:        true,
		UserKnownHostsFiles:   []string{filepath.Join(sshDir, "known_hosts"), filepath.Join(sshDir, "known_hosts2")},
		GlobalKnownHostsFiles: []string{"/etc/ssh/ssh_known_hosts", "/etc/ssh/ssh_known_hosts2"},
		StrictHostKeyChecking: "true",
		HostKeyAlias:          "dev-box",
		HashKnownHosts:        true,
		ProxyCommand:          "ssh -W %h:%p bastion",
		CertificateFiles:      []string{filepath.Join(sshDir, "work-cert.pub")},
	}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("parseSSHConfig:\n got %#v\nwant %#v", r, want)
	}
}

func TestParseSSHConfigDefaults(t *testing.T) {
	r, proxyJump := parseSSHConfig("dev", []byte("hostname dev\nidentitiesonly no\nhashknownhosts no\nstricthostkeychecking ask\nproxyjump a,b\n"), 22)
	if r.Addr != "dev:22" || r.IdentitiesOnly || r.HashKnownHosts || r.StrictHostKeyChecking != "ask" || proxyJump != "a,b" {
		t.Fatalf("parseSSHConfig = %#v, %q", r, proxyJump)
	}
	if r.UserKnownHostsFiles != nil || r.GlobalKnownHostsFiles != nil || r.ProxyCommand != "" || r.HostKeyAlias != "" {
		t.Fatalf("unset directives must stay empty: %#v", r)
	}
}

func TestResolveAlias_RejectsOptionLikeAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX ssh executable")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if _, err := ResolveRemote("-oProxyCommand=touch /tmp/pwned", 22); err == nil {
		t.Fatal("an alias starting with '-' was accepted")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ssh was run with an option-like alias")
	}
}

func TestResolveAlias_PassesAliasAfterDoubleDashAndReportsStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX ssh executable")
	}
	dir := t.TempDir()
	const fixture = `#!/bin/sh
if [ "$1" != -G ] || [ "$2" != -- ] || [ "$3" != dev ]; then
  echo "unexpected arguments: $*" >&2
  exit 255
fi
echo 'hostname dev.internal'
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fixture), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	r, err := ResolveRemote("dev", 22)
	if err != nil || r.Addr != "dev.internal:22" {
		t.Fatalf("ResolveRemote(dev) = %#v, %v", r, err)
	}
	_, err = ResolveRemote("other", 22)
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("error = %v, want ssh's stderr in it", err)
	}
}
