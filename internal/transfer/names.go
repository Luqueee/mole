package transfer

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode/utf8"
)

const (
	maxNameBytes  = 255
	maxSubdirs    = 8
	maxSubdirPath = 512
)

// CleanName validates a file name received from a peer and returns the name
// to create on this machine. A name is one path element: separators, "." and
// ".." are rejected rather than rewritten, so a malicious peer cannot steer a
// file out of the inbox. On Windows the characters that system forbids are
// replaced and reserved device names get a prefix.
func CleanName(name string) (string, error) {
	if name == "" || len(name) > maxNameBytes || !utf8.ValidString(name) {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("invalid file name %q: control character", name)
		}
	}
	if runtime.GOOS == "windows" {
		name = windowsSafe(name)
	}
	return name, nil
}

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// windowsSafe makes name creatable on Windows.
func windowsSafe(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" {
		name = "_"
	}
	stem, _, _ := strings.Cut(name, ".")
	if windowsReserved[strings.ToUpper(stem)] {
		name = "_" + name
	}
	return name
}

// CleanSubdir validates the optional sub-folder of the inbox a transfer asks
// for ("reports/2026"). Only relative paths made of plain names are allowed.
func CleanSubdir(to string) ([]string, error) {
	if to == "" {
		return nil, nil
	}
	if len(to) > maxSubdirPath {
		return nil, errors.New("destination folder is too long")
	}
	parts := strings.Split(strings.Trim(to, "/"), "/")
	if len(parts) > maxSubdirs {
		return nil, errors.New("destination folder is nested too deep")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		c, err := CleanName(p)
		if err != nil {
			return nil, fmt.Errorf("invalid destination folder %q", to)
		}
		out = append(out, c)
	}
	return out, nil
}
