// Package brew tells whether a binary is managed by Homebrew, so mole never
// overwrites one: Homebrew owns the file, its receipt and its upgrades.
package brew

import (
	"path/filepath"
	"strings"
)

// Formula returns the Homebrew formula that installed the binary at path, and
// whether it is one. Homebrew keeps every version below <prefix>/Cellar/<name>/
// <version>/, whatever the prefix is (/opt/homebrew, /usr/local, Linuxbrew),
// and links it into <prefix>/bin; callers should resolve symlinks first.
func Formula(path string) (string, bool) {
	p := filepath.ToSlash(path)
	_, rest, ok := strings.Cut(p, "/Cellar/")
	if !ok {
		return "", false
	}
	// <name>/<version>/<something>: a path with less is not a keg.
	if strings.Count(rest, "/") < 2 {
		return "", false
	}
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return "", false
	}
	return name, true
}

// Prefixes are the places Homebrew links binaries into, for programs that
// start without the shell's PATH (an app opened from the Finder).
var Prefixes = []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"}
