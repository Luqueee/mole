// Colour helpers for the CLI surface (usage, version, update). These
// share the same truecolor palette as the log viewer in logformat.go,
// so `mole` usage/version/update and `mole logs` use one visual
// language: dimmed metadata, bright content, coloured status accents.
package main

import (
	"fmt"
	"os"
)

// cliColor reports whether colourised output should be used for the
// given file: it must be a terminal and NO_COLOR must be unset.
func cliColor(f *os.File) bool {
	return isTerminal(f) && os.Getenv("NO_COLOR") == ""
}

// cBold wraps s in bold. Bold is a style, not a colour, so it uses the
// plain SGR code rather than fg().
func cBold(s string, on bool) string {
	if !on {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

// cDim — timestamp dim, the dimmest tier of the log palette.
func cDim(s string, on bool) string {
	if !on {
		return s
	}
	return fg(110, 114, 125, s)
}

// cGreen — FORWARD badge green (success).
func cGreen(s string, on bool) string {
	if !on {
		return s
	}
	return fg(63, 185, 80, s)
}

// cRed — ERROR badge red.
func cRed(s string, on bool) string {
	if !on {
		return s
	}
	return fg(248, 81, 73, s)
}

// cBlue — INFO badge blue.
func cBlue(s string, on bool) string {
	if !on {
		return s
	}
	return fg(56, 139, 253, s)
}

// cCyan — attr value: the log palette's light blue-gray.
func cCyan(s string, on bool) string {
	if !on {
		return s
	}
	return fg(173, 186, 199, s)
}

// cMagenta — bright log message colour (near-white).
func cMagenta(s string, on bool) string {
	if !on {
		return s
	}
	return fg(230, 237, 243, s)
}

// cGray — attr key dim.
func cGray(s string, on bool) string {
	if !on {
		return s
	}
	return fg(125, 133, 144, s)
}

// cYellow — WARN badge amber.
func cYellow(s string, on bool) string {
	if !on {
		return s
	}
	return fg(210, 153, 34, s)
}

// commandLine returns a styled "mole <cmd>" label for use in hints.
func commandLine(cmd string, on bool) string {
	return cCyan("mole "+cmd, on)
}

// separator prints a dim horizontal rule.
func separator(on bool) string {
	return cDim("─────────────────────────────────────────────────", on)
}

// banner returns the mole wordmark + tagline for the top of help output.
func banner(version string, on bool) string {
	name := cBold(cMagenta("mole", on), on)
	ver := cDim("v"+version, on)
	tag := cGray("local-port forwarder with auto-discover", on)
	return fmt.Sprintf("  %s  %s\n  %s", name, ver, tag)
}
