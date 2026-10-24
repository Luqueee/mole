package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestValidRef(t *testing.T) {
	for _, ok := range []string{"main", "feat/mesh-nodes", "release/v0.3.0", "v0.1.1", "user@fork/x", "a.b_c+d"} {
		if err := validRef(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "--upload-pack=x", "a b", "a;b", "a$(id)", "a`id`", "../x", "a/../b", "a\nb", "a'b", `a"b`, "a*", strings.Repeat("a", 201)} {
		if err := validRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestInstallerURL(t *testing.T) {
	t.Setenv("MOLE_RAW_URL", "")
	if got := installerURL("install.sh", ""); got != "https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh" {
		t.Errorf("default: %s", got)
	}
	if got := installerURL("install.ps1", "feat/x"); got != "https://raw.githubusercontent.com/Luqueee/mole/refs/heads/feat/x/scripts/install.ps1" {
		t.Errorf("branch: %s", got)
	}
	t.Setenv("MOLE_RAW_URL", "file:///tmp/raw/")
	if got := installerURL("install.sh", "a/b"); got != "file:///tmp/raw/refs/heads/a/b/scripts/install.sh" {
		t.Errorf("override: %s", got)
	}
}

func TestUpdateEnv(t *testing.T) {
	has := func(env []string, kv string) bool {
		for _, e := range env {
			if e == kv {
				return true
			}
		}
		return false
	}
	has2 := func(env []string, prefix string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}
	if env := updateEnv("/bin/mole", "", ""); !has(env, "INSTALL_DIR=/bin/mole") || has2(env, "MOLE_VERSION=") || has2(env, "MOLE_BRANCH=") {
		t.Errorf("latest: %v", env)
	}
	if env := updateEnv("/bin/mole", "v0.1.1", ""); !has(env, "MOLE_VERSION=v0.1.1") {
		t.Errorf("version: %v", env)
	}
	env := updateEnv("/bin/mole", "", "feat/x")
	if runtime.GOOS == "windows" {
		if !has(env, "MOLE_BRANCH=feat/x") || has2(env, "MOLE_VERSION=") {
			t.Errorf("branch on windows: %v", env)
		}
	} else if !has(env, "MOLE_VERSION=feat/x") || has2(env, "MOLE_BRANCH=") {
		t.Errorf("branch on unix: %v", env)
	}
}

func TestBuildUpdateCommandDownloadsToAFileAndChecksIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Unix installer command")
	}
	t.Setenv("MOLE_RAW_URL", "")
	cmd, err := buildUpdateCommand("/bin/mole", "", "feat/x", true)
	if err != nil {
		t.Skip("no curl or wget here:", err)
	}
	script := cmd.Args[len(cmd.Args)-1]
	for _, want := range []string{
		"refs/heads/feat/x/scripts/install.sh",                                 // the branch's own installer
		`mktemp`, `sh "$tmp" --no-verify`, "rc=$?", `rm -f "$tmp"`, "exit $rc", // a failed download is a failure
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "| sh") {
		t.Errorf("piping into sh hides a failed download:\n%s", script)
	}
	cmd, _ = buildUpdateCommand("/bin/mole", "", "", false)
	if script := cmd.Args[len(cmd.Args)-1]; !strings.Contains(script, "/main/scripts/install.sh") || strings.Contains(script, "--no-verify") {
		t.Errorf("latest: %s", script)
	}
}

func TestSourceRef(t *testing.T) {
	if got := sourceRef("", "feat/x"); got != "branch feat/x" {
		t.Errorf("branch: %q", got)
	}
	if got := sourceRef("v0.1.1", ""); got != "v0.1.1" {
		t.Errorf("version: %q", got)
	}
}
