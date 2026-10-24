package main

import (
	"strings"
	"testing"
)

func TestFormatBuild(t *testing.T) {
	for _, c := range []struct{ ref, commit, want string }{
		{"feat/mesh-nodes", "a1b2c3d", "feat/mesh-nodes@a1b2c3d"},
		{"main", "", "main"},
		{"", "a1b2c3d+dirty", "a1b2c3d+dirty"},
		{"", "", ""},
		{" v0.3.0 ", " abc ", "v0.3.0@abc"},
	} {
		if got := formatBuild(c.ref, c.commit); got != c.want {
			t.Errorf("formatBuild(%q, %q) = %q, want %q", c.ref, c.commit, got, c.want)
		}
	}
}

func TestVersionLineShowsWhereItWasBuiltFrom(t *testing.T) {
	oldRef, oldCommit := buildRef, buildCommit
	defer func() { buildRef, buildCommit = oldRef, oldCommit }()

	buildRef, buildCommit = "feat/x", "abc1234"
	if got := versionLine(false); got != "mole v"+version+" (feat/x@abc1234)" {
		t.Errorf("stamped build: %q", got)
	}
	if got := versionLine(true); !strings.Contains(got, "feat/x@abc1234") || !strings.Contains(got, "\x1b[") {
		t.Errorf("coloured: %q", got)
	}
	// With no stamp the line still starts "mole vX" (node.go parses it) and
	// never contains a stray "()".
	buildRef, buildCommit = "", ""
	got := versionLine(false)
	if !strings.HasPrefix(got, "mole v"+version) || strings.Contains(got, "()") {
		t.Errorf("unstamped build: %q", got)
	}
	if v := parseVersionOutput(versionLine(false)); v != version {
		t.Errorf("parseVersionOutput(%q) = %q", versionLine(false), v)
	}
	buildRef, buildCommit = "feat/x", "abc1234"
	if v := parseVersionOutput(versionLine(true)); v != version {
		t.Errorf("a stamped line must still parse: %q", v)
	}
}
