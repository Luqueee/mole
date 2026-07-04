// mole config — utilities for the active mole.yaml.
//
// Subcommands:
//
//	mole config edit [-config PATH] [-editor CMD]
//	    Opens the active config in the user's editor. Resolution
//	    order is identical to `mole up`: explicit -config, then
//	    ./mole.yaml, then the user-global config. If the resolved
//	    file doesn't exist yet, a fresh one populated with the
//	    package defaults is created first so the editor always
//	    has something to write back to.
//
//	    The editor is chosen as: -editor flag > $VISUAL > $EDITOR
//	    > "vi". The editor is exec'd with the config path as its
//	    only positional argument. Its exit code is propagated.
//
//	    The intent is to make editing the config as friction-free
//	    as possible without spawning a TUI or maintaining an
//	    in-process editor. After saving the file, the user runs
//	    `mole restart` (or `mole up -d` if nothing is running)
//	    to pick the changes up.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Luqueee/mole/internal/config"
)

func runConfig(args []string) int {
	if len(args) == 0 {
		printConfigUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "edit":
		return runConfigEdit(args[1:])
	case "-h", "--help", "help":
		printConfigUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mole config: unknown subcommand: %s\n\n", args[0])
		printConfigUsage(os.Stderr)
		return 2
	}
}

func printConfigUsage(w *os.File) {
	color := cliColor(w)
	fmt.Fprintf(w, "%s\n\n", cBold("mole config — utilities for mole.yaml", color))
	fmt.Fprintf(w, "  %s\n", cBold("USAGE", color))
	fmt.Fprintf(w, "    mole config %s\n", cDim("<subcommand> [flags]", color))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", cBold("SUBCOMMANDS", color))
	fmt.Fprintf(w, "    %s  open the active mole.yaml in $VISUAL / $EDITOR / vi\n", cGreen("edit", color))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", cBold("NOTES", color))
	fmt.Fprintf(w, "    %s\n", cDim("  After saving the file, run `mole restart` to apply changes.", color))
}

// runConfigEdit opens the resolved config in the chosen editor. If
// the file doesn't exist, it materialises one with the package
// defaults so the editor always has a writable target.
func runConfigEdit(args []string) int {
	fs := flag.NewFlagSet("config edit", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	editor := fs.String("editor", "", "editor to invoke (default: $VISUAL, $EDITOR, then vi)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole config edit [flags]

Opens the active mole.yaml in your editor. If the file does not
exist yet, a fresh one is written with the default config first.

Flags:
  -config       path to YAML config (default: ./mole.yaml, then user-global)
  -editor       editor command (default: $VISUAL, $EDITOR, then vi)`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path, err := resolveOrCreateConfigPath(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}

	// If the file doesn't exist, materialise a defaults-only YAML
	// so the editor has something to write to. This matches the
	// behaviour of `mole up` reading defaults when the file is
	// missing, but for the editor case we want the file to
	// exist on disk so the user can save their changes.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// Don't clobber an explicit -config target that just
		// doesn't exist yet. Create the parent dir if needed.
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mole config edit: mkdir %s: %v\n", filepath.Dir(path), err)
			return 1
		}
		def := config.Default()
		if err := config.Save(path, def); err != nil {
			fmt.Fprintf(os.Stderr, "mole config edit: write %s: %v\n", path, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "mole config edit: created %s with defaults\n", path)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "mole config edit: stat %s: %v\n", path, err)
		return 1
	}

	editorCmd := pickEditor(*editor)
	if editorCmd == "" {
		fmt.Fprintln(os.Stderr, "mole config edit: no editor found; set $VISUAL or $EDITOR, or pass -editor")
		return 1
	}

	// We exec the editor in a way that lets the user's terminal
	// see it: inherit stdin/stdout/stderr so editors like vi or
	// nano that need a TTY work. The editor's exit code is what
	// we return — a non-zero exit is the user signalling a
	// problem (e.g. E212 in vim = "can't open file for writing").
	cmd := exec.Command(editorCmd, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Preserve the editor's exit code so the user's
			// tooling (CI, scripts) can react.
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "mole config edit: %s %s: %v\n", editorCmd, path, err)
		return 1
	}
	return 0
}

// pickEditor chooses the editor command in this order:
//
//  1. The value of editorOverride, if non-empty (the -editor flag).
//  2. $VISUAL.
//  3. $EDITOR.
//  4. "vi" as a last-resort default that POSIX guarantees exists.
//
// We do NOT call exec.LookPath on "vi" because the user might
// have configured something different and we want the same
// fallback regardless of whether the binary is on PATH at
// exec-time vs. the moment we resolve it. LookPath is a side
// effect we don't need.
//
// The returned string is what we'll pass to exec.Command; it may
// contain arguments (e.g. "code --wait"). We split on the first
// space so the editor is the program and the rest are args — but
// only at exec time. Returning the whole string keeps the
// function pure and easy to test.
func pickEditor(editorOverride string) string {
	if editorOverride != "" {
		return editorOverride
	}
	if v := os.Getenv("VISUAL"); v != "" {
		return v
	}
	if e := os.Getenv("EDITOR"); e != "" {
		return e
	}
	return "vi"
}
