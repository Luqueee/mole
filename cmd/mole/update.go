// `mole update` — self-update by re-running the official installer.
//
// The Unix installer builds from source; the Windows installer downloads a
// verified release archive unless run from a local clone. Update
// re-runs the very same installer, but pins the destination to *this*
// binary's own path via INSTALL_DIR so the running copy is replaced in
// place — wherever it happens to live.
//
// Usage:
//
//	mole update                 # update to latest main (Unix) / release (Windows)
//	mole update -version v0.1.0  # pin a specific version
//	mole update -branch feat/x   # build and install a branch, to try it out
//	mole update -dry-run         # print the command without running it
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Luqueee/mole/internal/brew"
)

// defaultRawURL is where the installers are fetched from; MOLE_RAW_URL points
// elsewhere (a fork, or a local directory through file://, for tests).
const defaultRawURL = "https://raw.githubusercontent.com/Luqueee/mole"

// installerURL is the URL of an installer script: the one on main, or, for a
// branch, the one that branch carries (a branch can change the installer too).
func installerURL(script, branch string) string {
	base := strings.TrimRight(os.Getenv("MOLE_RAW_URL"), "/")
	if base == "" {
		base = defaultRawURL
	}
	if branch != "" {
		return base + "/refs/heads/" + branch + "/scripts/" + script
	}
	return base + "/main/scripts/" + script
}

func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	var (
		ref      = fs.String("version", "", "version to install (default: latest)")
		branch   = fs.String("branch", "", "git branch (or tag) to build from source and install")
		dryRun   = fs.Bool("dry-run", false, "print the installer command without running it")
		noVerify = fs.Bool("no-verify", false, "skip the post-install version check")
	)
	fs.Usage = func() {
		c := cliColor(os.Stderr)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, banner(version, c))
		fmt.Fprintf(os.Stderr, "\n  %s\n", cBold("USAGE", c))
		fmt.Fprintf(os.Stderr, "    %s\n", commandLine("update [flags]", c))
		fmt.Fprintf(os.Stderr, "\n  %s\n", cBold("DESCRIPTION", c))
		fmt.Fprintln(os.Stderr, "    Updates mole in place by re-running the official installer")
		fmt.Fprintln(os.Stderr, "    against this binary's own location.")
		fmt.Fprintf(os.Stderr, "\n  %s\n", cBold("FLAGS", c))
		fmt.Fprintf(os.Stderr, "    %s  %s\n", cGreen("  -version <ref>", c), "version to install (Windows: release tag; Unix: git ref)")
		fmt.Fprintf(os.Stderr, "    %s  %s\n", cGreen("  -branch <name>", c), "build that branch (or tag) from source and install it (needs git; Go is fetched when missing)")
		fmt.Fprintf(os.Stderr, "    %s  %s\n", cGreen("  -dry-run", c), "print the installer command instead of running it")
		fmt.Fprintf(os.Stderr, "    %s  %s\n", cGreen("  -no-verify", c), "skip the installer's post-install version check")
		fmt.Fprintf(os.Stderr, "\n  %s\n", cDim("Requires 'git' and 'curl'/'wget' on Unix (Go is downloaded if missing), or PowerShell on Windows,", c))
		fmt.Fprintf(os.Stderr, "  %s\n", cDim("plus network access to github.com.", c))
		fmt.Fprintln(os.Stderr)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	color := cliColor(os.Stdout)
	if *branch != "" && *ref != "" {
		fmt.Fprintln(os.Stderr, cRed("  ✗ update:", color), "-branch and -version are alternatives; give one")
		return 2
	}
	if *branch != "" {
		if err := validRef(*branch); err != nil {
			fmt.Fprintln(os.Stderr, cRed("  ✗ update:", color), err)
			return 2
		}
	}

	// Resolve the running binary and follow any symlinks so we overwrite
	// the real file, not a link into it.
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, cRed("  ✗ update:", color), "cannot locate current binary:", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// A binary Homebrew installed belongs to Homebrew: replacing it would leave
	// its receipt and its upgrades describing a file that is no longer theirs.
	if formula, ok := brew.Formula(exe); ok {
		fmt.Fprintln(os.Stderr, cRed("  ✗ update:", color), "this mole was installed by Homebrew; it updates it, not mole")
		fmt.Fprintf(os.Stderr, "        brew upgrade %s\n", formula)
		return 1
	}

	// The Unix installer builds from source and fetches Go itself when it is
	// missing. The Windows installer uses a release archive and needs Go only
	// for MOLE_SRC or a branch.
	if runtime.GOOS == "windows" && (os.Getenv("MOLE_SRC") != "" || *branch != "") {
		if _, err := exec.LookPath("go"); err != nil {
			fmt.Fprintln(os.Stderr, cRed("  ✗ update:", color), "'go' is not installed or not on PATH.")
			fmt.Fprintln(os.Stderr, "        Install Go 1.26.4+ from https://go.dev/dl/ and re-run.")
			return 1
		}
	}

	cmd, err := buildUpdateCommand(exe, *ref, *branch, *noVerify)
	if err != nil {
		fmt.Fprintln(os.Stderr, cRed("  ✗ update:", color), err)
		return 1
	}

	if *dryRun {
		fmt.Printf("\n  %s\n", cDim("─ dry-run ─", color))
		fmt.Printf("  %s  %s %s\n", cDim("would run:", color), cCyan(cmd.Path, color), cCyan(quoteArgs(cmd.Args[1:]), color))
		for _, kv := range updateEnv(exe, *ref, *branch) {
			if k, v, _ := strings.Cut(kv, "="); k != "MOLE_RELEASE_ONLY" {
				fmt.Printf("  %s %s\n", cDim(k+" =", color), cBold(v, color))
			}
		}
		fmt.Println()
		return 0
	}

	fmt.Printf("\n  %s %s %s %s\n",
		cBold(cMagenta("mole", color), color),
		cDim("v"+version, color),
		cCyan("→", color),
		cBold(sourceRef(*ref, *branch), color))
	fmt.Printf("  %s  %s\n", cDim("target", color), cBlue(exe, color))
	fmt.Println()

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, cRed("  ✗ update failed:", color), err)
		return 1
	}
	fmt.Printf("  %s\n\n", cGreen("✓ mole updated successfully", color))
	return 0
}

// validRef checks a git ref is something safe to hand to the installers (it
// ends up in a URL, a `git clone --branch` and a linker flag).
func validRef(ref string) error {
	if ref == "" || len(ref) > 200 || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	for _, r := range ref {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/@+-", r)) {
			return fmt.Errorf("invalid git ref %q: only letters, digits and . _ / @ + - are allowed", ref)
		}
	}
	return nil
}

// buildUpdateCommand assembles the platform-specific installer invocation.
// INSTALL_DIR pins the destination to the current binary's path. ref is a
// version (a git ref on Unix, a release tag on Windows) and branch a git
// branch to build from source (on Windows through MOLE_BRANCH, since
// MOLE_VERSION means a release there). The parent environment is inherited
// so GO, MOLE_SRC, MOLE_REPO and friends still work.
func buildUpdateCommand(dest, ref, branch string, noVerify bool) (*exec.Cmd, error) {
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
			"$ErrorActionPreference = 'Stop'; & ([scriptblock]::Create((irm %s))) %s",
			installerURL("install.ps1", branch), verify,
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
			shArgs = " --no-verify"
		}
		// Download to a file and run that: piped into sh, a failed download
		// (a branch that does not exist) would hand sh an empty script, which
		// "succeeds".
		script := fmt.Sprintf(`tmp="$(mktemp)" && %s && sh "$tmp"%s; rc=$?; rm -f "$tmp"; exit $rc`,
			fetch(installerURL("install.sh", branch), `"$tmp"`), shArgs)
		cmd = exec.Command("sh", "-c", script)
	}

	cmd.Env = append(os.Environ(), updateEnv(dest, ref, branch)...)
	return cmd, nil
}

// updateEnv is what the installer is told through the environment: where to
// put the binary, and which version or branch to install.
func updateEnv(dest, ref, branch string) []string {
	env := []string{"INSTALL_DIR=" + dest}
	if runtime.GOOS == "windows" {
		env = append(env, "MOLE_RELEASE_ONLY=1")
	}
	switch {
	case branch != "" && runtime.GOOS == "windows":
		env = append(env, "MOLE_BRANCH="+branch)
	case branch != "":
		env = append(env, "MOLE_VERSION="+branch)
	case ref != "":
		env = append(env, "MOLE_VERSION="+ref)
	}
	return env
}

// fetchCommand returns a function that renders a command downloading a URL
// into a file, using curl or else wget.
func fetchCommand() (func(url, out string) string, error) {
	if _, err := exec.LookPath("curl"); err == nil {
		return func(url, out string) string { return fmt.Sprintf("curl -fsSL -o %s '%s'", out, url) }, nil
	}
	if _, err := exec.LookPath("wget"); err == nil {
		return func(url, out string) string { return fmt.Sprintf("wget -qO %s '%s'", out, url) }, nil
	}
	return nil, fmt.Errorf("neither 'curl' nor 'wget' found on PATH; cannot download the installer")
}

func powershellPath() (string, error) {
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("PowerShell not found on PATH; cannot run the Windows installer")
}

func sourceRef(ref, branch string) string {
	if branch != "" {
		return "branch " + branch
	}
	if ref == "" {
		if runtime.GOOS == "windows" {
			return "latest release"
		}
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
