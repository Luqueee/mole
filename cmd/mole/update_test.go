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
	// The latest release: no version, and never a clone in the working directory.
	if env := updateEnv("/bin/mole", "", ""); !envHas(env, "INSTALL_DIR=/bin/mole") || !envHas(env, "MOLE_RELEASE_ONLY=1") || envHasPrefix(env, "MOLE_VERSION=") || envHasPrefix(env, "MOLE_BRANCH=") {
		t.Errorf("latest: %v", env)
	}
	if env := updateEnv("/bin/mole", "v0.1.1", ""); !envHas(env, "MOLE_VERSION=v0.1.1") {
		t.Errorf("version: %v", env)
	}
	env := updateEnv("/bin/mole", "", "feat/x")
	if runtime.GOOS == "windows" {
		if !envHas(env, "MOLE_BRANCH=feat/x") || envHasPrefix(env, "MOLE_VERSION=") {
			t.Errorf("branch on windows: %v", env)
		}
	} else if !envHas(env, "MOLE_VERSION=feat/x") || envHasPrefix(env, "MOLE_BRANCH=") {
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
	if !envHas(cmd.Env, "MOLE_RELEASE_ONLY=1") || envHasPrefix(cmd.Env, "MOLE_VERSION=") {
		t.Errorf("latest: the installer is not told to install the latest release: %v", cmd.Env[len(cmd.Env)-2:])
	}
}

func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func envHasPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func TestIsReleaseTag(t *testing.T) {
	for _, ok := range []string{"v0.1.0", "v0.6.1", "v10.20.30"} {
		if !isReleaseTag(ok) {
			t.Errorf("%q is a release tag", ok)
		}
	}
	for _, bad := range []string{"", "main", "0.6.1", "v0.6", "v0.6.1-rc1", "desktop-v0.8.4", "v0.6.1\n", "feat/v0.6.1", "v0.6.1.2"} {
		if isReleaseTag(bad) {
			t.Errorf("%q is not a release tag", bad)
		}
	}
}

func TestSourceRef(t *testing.T) {
	for _, c := range []struct{ ref, branch, want string }{
		{"", "", "latest release"},
		{"", "feat/x", "branch feat/x"},
		{"v0.1.1", "", "release v0.1.1"},
		{"main", "", "main (built from source)"},
	} {
		if got := sourceRef(c.ref, c.branch); got != c.want {
			t.Errorf("sourceRef(%q, %q) = %q, want %q", c.ref, c.branch, got, c.want)
		}
	}
}

// A dry run says what it would install, and changes nothing.
func TestUpdateDryRunSaysWhatItInstalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Unix installer command")
	}
	if _, err := fetchCommand(); err != nil {
		t.Skip("no curl or wget here:", err)
	}
	t.Setenv("MOLE_RAW_URL", "")
	t.Setenv("NO_COLOR", "1")
	for _, c := range []struct {
		args []string
		want []string
		not  []string
	}{
		{nil, []string{"would install:  latest release", "/main/scripts/install.sh", "INSTALL_DIR ="}, []string{"MOLE_VERSION", "MOLE_RELEASE_ONLY", "main)"}},
		{[]string{"-version", "v0.6.1"}, []string{"would install:  release v0.6.1", "MOLE_VERSION = v0.6.1"}, nil},
		{[]string{"-branch", "feat/x"}, []string{"would install:  branch feat/x", "refs/heads/feat/x/scripts/install.sh", "MOLE_VERSION = feat/x"}, nil},
	} {
		var code int
		out, _ := capture(t, func() { code = runUpdate(append(c.args, "-dry-run")) })
		if code != 0 {
			t.Errorf("%v: exit %d\n%s", c.args, code, out)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: dry run lacks %q:\n%s", c.args, w, out)
			}
		}
		for _, n := range c.not {
			if strings.Contains(out, n) {
				t.Errorf("%v: dry run shows %q:\n%s", c.args, n, out)
			}
		}
	}
}

func TestUpdateRejectsABadVersion(t *testing.T) {
	var code int
	_, errOut := capture(t, func() { code = runUpdate([]string{"-version", "a;b", "-dry-run"}) })
	if code != 2 || !strings.Contains(errOut, "invalid git ref") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}
