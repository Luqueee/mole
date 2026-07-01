// `mole update` — self-update by re-running the official installer.
//
// mole is distributed as a source install: scripts/install.sh (Unix) and
// scripts/install.ps1 (Windows) clone the latest source, `go build` it,
// and copy the binary into place. Rather than reinvent that logic, update
// re-runs the very same installer, but pins the destination to *this*
// binary's own path via INSTALL_DIR so the running copy is replaced in
// place — wherever it happens to live.
//
// Usage:
//
//	mole update                 # update to the latest main
//	mole update -version v0.2.0  # pin a specific git ref
//	mole update -dry-run         # print the command without running it
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	installShURL = "https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh"
	installPsURL = "https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.ps1"
)

func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	var (
		ref      = fs.String("version", "", "git ref to install (branch, tag, or commit; default: latest main)")
		dryRun   = fs.Bool("dry-run", false, "print the installer command without running it")
		noVerify = fs.Bool("no-verify", false, "skip the post-install version check")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole update [flags]

Updates mole in place by re-running the official installer against this
binary's own location. The installer clones the latest source, builds it
with Go, and replaces the current executable.

Flags:
  -version <ref>   git ref to install (branch, tag, or commit; default: main)
  -dry-run         print the installer command instead of running it
  -no-verify       skip the installer's post-install version check

Requires 'go' and either 'curl'/'wget' (Unix) or PowerShell (Windows),
plus network access to github.com.`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Resolve the running binary and follow any symlinks so we overwrite
	// the real file, not a link into it.
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "update: cannot locate current binary:", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// The installer needs 'go' to build from source; fail early with a
	// clear message rather than deep inside the shell/PowerShell pipeline.
	if _, err := exec.LookPath("go"); err != nil {
		fmt.Fprintln(os.Stderr, "update: 'go' is not installed or not on PATH.")
		fmt.Fprintln(os.Stderr, "        Install Go 1.22+ from https://go.dev/dl/ and re-run.")
		return 1
	}

	cmd, err := buildUpdateCommand(exe, *ref, *noVerify)
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}

	if *dryRun {
		fmt.Println("would run:", cmd.Path, quoteArgs(cmd.Args[1:]))
		fmt.Println("  INSTALL_DIR =", exe)
		if *ref != "" {
			fmt.Println("  MOLE_VERSION =", *ref)
		}
		return 0
	}

	fmt.Printf("mole %s → updating from %s\n", version, sourceRef(*ref))
	fmt.Println("  target:", exe)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "update: installer failed:", err)
		return 1
	}
	return 0
}

// buildUpdateCommand assembles the platform-specific installer invocation.
// INSTALL_DIR pins the destination to the current binary's path; MOLE_VERSION
// pins the git ref when one is given. The parent environment is inherited so
// GO, MOLE_SRC, and friends still work.
func buildUpdateCommand(dest, ref string, noVerify bool) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		verify := ""
		if noVerify {
			verify = " -NoVerify"
		}
		// Download the installer and pipe it to Invoke-Expression, the
		// same one-liner documented in the README, run via a nested
		// PowerShell so args like -NoVerify are honoured.
		script := fmt.Sprintf(
			"& ([scriptblock]::Create((irm %s))) %s",
			installPsURL, verify,
		)
		ps, err := powershellPath()
		if err != nil {
			return nil, err
		}
		cmd = exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	} else {
		fetch, err := fetchCommand()
		if err != nil {
			return nil, err
		}
		shArgs := ""
		if noVerify {
			shArgs = " -s -- --no-verify"
		}
		// e.g. curl -fsSL <url> | sh -s -- --no-verify
		script := fmt.Sprintf("%s %s | sh%s", fetch, installShURL, shArgs)
		cmd = exec.Command("sh", "-c", script)
	}

	env := os.Environ()
	env = append(env, "INSTALL_DIR="+dest)
	if ref != "" {
		env = append(env, "MOLE_VERSION="+ref)
	}
	cmd.Env = env
	return cmd, nil
}

// fetchCommand returns a downloader invocation that writes to stdout,
// preferring curl and falling back to wget.
func fetchCommand() (string, error) {
	if _, err := exec.LookPath("curl"); err == nil {
		return "curl -fsSL", nil
	}
	if _, err := exec.LookPath("wget"); err == nil {
		return "wget -qO-", nil
	}
	return "", fmt.Errorf("neither 'curl' nor 'wget' found on PATH; cannot download the installer")
}

func powershellPath() (string, error) {
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("PowerShell not found on PATH; cannot run the Windows installer")
}

func sourceRef(ref string) string {
	if ref == "" {
		return "latest (main)"
	}
	return ref
}

// quoteArgs renders a slice of args as a single space-joined string for
// display only (dry-run output).
func quoteArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
