package tunnel

import (
	"os"
	"path/filepath"
	"runtime"
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
