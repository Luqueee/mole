package main

import (
	"runtime/debug"
	"strings"
)

// Set by the installers (-ldflags "-X main.buildRef=... -X main.buildCommit=...")
// so a binary built from a branch says so.
var (
	buildRef    string // the git ref it was built from: a branch, a tag
	buildCommit string // that ref's commit, short; "+dirty" if the tree had changes
)

// buildLabel says where this binary came from, e.g. "feat/mesh-nodes@a1b2c3d",
// or "" for a build that carries no such information. Without the installer's
// stamp it falls back to what `go build` records inside a git checkout.
func buildLabel() string {
	commit := buildCommit
	if commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			modified := false
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					commit = s.Value
					if len(commit) > 7 {
						commit = commit[:7]
					}
				case "vcs.modified":
					modified = s.Value == "true"
				}
			}
			if commit != "" && modified {
				commit += "+dirty"
			}
		}
	}
	return formatBuild(buildRef, commit)
}

func formatBuild(ref, commit string) string {
	ref, commit = strings.TrimSpace(ref), strings.TrimSpace(commit)
	switch {
	case ref != "" && commit != "":
		return ref + "@" + commit
	case ref != "":
		return ref
	}
	return commit
}

// versionLine is what `mole version` prints.
func versionLine(color bool) string {
	line := cBold(cMagenta("mole", color), color) + " " + cDim("v"+version, color)
	if b := buildLabel(); b != "" {
		line += " " + cDim("("+b+")", color)
	}
	return line
}
