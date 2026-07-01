// Colour helpers for the CLI surface (usage, version, update). The log
// viewer has its own truecolor machinery in logformat.go; these are the
// simpler standard-ANSI codes used for one-shot CLI output.
package main

import (
	"fmt"
	"os"
)

// ANSI codes — intentionally basic 16-colour so they render correctly
// even on terminals without truecolor support.
const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiBlue    = "\x1b[34m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"
	ansiGray    = "\x1b[90m"
)

// cliColor reports whether colourised output should be used for the
// given file: it must be a terminal and NO_COLOR must be unset.
func cliColor(f *os.File) bool {
	return isTerminal(f) && os.Getenv("NO_COLOR") == ""
}

// paint wraps s in the given ANSI codes only when color is true.
func paint(color, s string, colorOn bool) string {
	if !colorOn {
		return s
	}
	return color + s + ansiReset
}

// Convenience wrappers for the most common styles.
func cBold(s string, on bool) string   { return paint(ansiBold, s, on) }
func cDim(s string, on bool) string    { return paint(ansiDim, s, on) }
func cGreen(s string, on bool) string  { return paint(ansiGreen, s, on) }
func cRed(s string, on bool) string    { return paint(ansiRed, s, on) }
func cYellow(s string, on bool) string { return paint(ansiYellow, s, on) }
func cBlue(s string, on bool) string   { return paint(ansiBlue, s, on) }
func cCyan(s string, on bool) string   { return paint(ansiCyan, s, on) }
func cMagenta(s string, on bool) string { return paint(ansiMagenta, s, on) }
func cGray(s string, on bool) string   { return paint(ansiGray, s, on) }

// commandLine returns a styled "mole <cmd>" label for use in hints.
func commandLine(cmd string, on bool) string {
	return cGreen("mole "+cmd, on)
}

// separator prints a dim horizontal rule.
func separator(on bool) string {
	if !on {
		return "─────────────────────────────────────────────────"
	}
	return cDim("─────────────────────────────────────────────────", on)
}

// banner returns the mole wordmark + tagline for the top of help output.
func banner(version string, on bool) string {
	name := cBold(cMagenta("mole", on), on)
	ver := cDim("v"+version, on)
	tag := cDim("local-port forwarder with auto-discover", on)
	return fmt.Sprintf("  %s  %s\n  %s", name, ver, tag)
}
