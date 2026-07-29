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
