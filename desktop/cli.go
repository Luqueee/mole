package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Luqueee/mole/internal/brew"
)

// The mole CLI is embedded so the app can install it. Builds that skip
// `wails3 task bundle:cli` only have the README here and report Bundled=false.
//
//go:embed all:embedded
var embeddedFS embed.FS

func cliName() string {
	if runtime.GOOS == "windows" {
		return "mole.exe"
	}
	return "mole"
}

// bundledCLI returns the embedded binary, or nil when this build has none.
func bundledCLI() []byte {
	b, err := embeddedFS.ReadFile("embedded/" + cliName())
	if err != nil {
		return nil
	}
	return b
}

// cliInstallDir is where InstallCLI puts the binary: a per-user directory,
// so no elevated rights are needed.
func cliInstallDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "mole", "bin")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin")
}

// brewDirs are searched after PATH and the install directory: an app opened
// from the Finder or the Start menu does not inherit the shell's PATH, which is
// where Homebrew's links live. A variable so tests can point it elsewhere.
var brewDirs = brew.Prefixes

// moleIdentity is what this project's `mole version` starts with. Another
// program may be called mole too (homebrew-core has one that cleans a Mac and
// answers "Mole version 1.44.1"): shelling out to it would run the wrong tool.
var moleIdentity = regexp.MustCompile(`(?m)^mole v\d+\.\d+\.\d+`)

type identityVerdict struct {
	size int64
	mod  time.Time
	ok   bool
}

var (
	identityMu    sync.Mutex
	identityCache = map[string]identityVerdict{}
)

// isMole reports whether the file at bin is this project's CLI. The answer is
// cached until the file changes: moleBin runs on every action.
func isMole(bin string) bool {
	fi, err := os.Stat(bin)
	if err != nil || fi.IsDir() {
		return false
	}
	identityMu.Lock()
	v, hit := identityCache[bin]
	identityMu.Unlock()
	if hit && v.size == fi.Size() && v.mod.Equal(fi.ModTime()) {
		return v.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "version").Output() // stdin is /dev/null
	ok := moleIdentity.Match(out)
	identityMu.Lock()
	identityCache[bin] = identityVerdict{size: fi.Size(), mod: fi.ModTime(), ok: ok}
	identityMu.Unlock()
	return ok
}

// moleBin finds the CLI the app shells out to: the first binary on PATH, in the
// install directory or in a Homebrew prefix that says it is this project's.
func moleBin() (string, error) {
	dirs := append(filepath.SplitList(os.Getenv("PATH")), cliInstallDir())
	dirs = append(dirs, brewDirs...)
	seen := map[string]bool{}
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if p := filepath.Join(d, cliName()); isMole(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("mole CLI not found: install it from the Setup tab")
}

// brewFormulaOf returns the Homebrew formula that owns the file bin points at.
func brewFormulaOf(bin string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	return brew.Formula(bin)
}

// CLIStatus describes the CLI install for the Setup tab.
type CLIStatus struct {
	Bundled    bool   `json:"bundled"`    // this build embeds a CLI
	Installed  bool   `json:"installed"`  // a mole binary was found
	Path       string `json:"path"`       // where it was found
	Version    string `json:"version"`    // its `mole version` output
	UpToDate   bool   `json:"upToDate"`   // identical to the embedded one
	InstallDir string `json:"installDir"` // where Install puts it
	DirOnPath  bool   `json:"dirOnPath"`  // InstallDir is in the shell PATH
	PathSet    bool   `json:"pathSet"`    // the shell profile already adds InstallDir
	Profile    string `json:"profile"`    // file AddCLIToPath edits ("user PATH" on Windows)
	ManagedBy  string `json:"managedBy"`  // the Homebrew formula that owns the binary, or ""
}

// GetCLI reports whether the CLI is installed and whether it matches the
// one embedded in this app.
func (m *MoleService) GetCLI() CLIStatus {
	dir := cliInstallDir()
	st := CLIStatus{Bundled: bundledCLI() != nil, InstallDir: dir}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			st.DirOnPath = true
		}
	}
	st.Profile, st.PathSet = pathProfile()
	bin, err := moleBin()
	if err != nil {
		return st
	}
	st.Installed, st.Path = true, bin
	st.ManagedBy, _ = brewFormulaOf(bin)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "version").Output(); err == nil {
		st.Version = strings.TrimSpace(string(out))
	}
	if want := bundledCLI(); want != nil {
		if have, err := os.ReadFile(bin); err == nil {
			st.UpToDate = sha256.Sum256(have) == sha256.Sum256(want)
		}
	}
	return st
}

// InstallCLI writes the embedded CLI into the install directory, replacing
// any previous copy there. A running daemon keeps its old binary until it is
// restarted.
func (m *MoleService) InstallCLI() error {
	// Homebrew owns its copy: a second one here would shadow it on PATH.
	if cur, err := moleBin(); err == nil {
		if formula, ok := brewFormulaOf(cur); ok {
			return fmt.Errorf("mole is installed by Homebrew: update it with `brew upgrade %s`", formula)
		}
	}
	bin := bundledCLI()
	if bin == nil {
		return fmt.Errorf("this build does not embed the CLI")
	}
	dir := cliInstallDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, cliName())
	tmp, err := os.CreateTemp(dir, ".mole-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	// Rename over the target: replacing a binary in use can't be done by
	// writing into it.
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	return nil
}

const pathMarker = "# added by mole"

// shellProfile is the startup file of the user's login shell and the line
// that puts the install directory on PATH.
func shellProfile() (file, line string) {
	home, _ := os.UserHomeDir()
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh":
		return filepath.Join(home, ".zshrc"), `export PATH="$HOME/.local/bin:$PATH"`
	case "bash":
		if runtime.GOOS == "darwin" { // login shells there skip .bashrc
			return filepath.Join(home, ".bash_profile"), `export PATH="$HOME/.local/bin:$PATH"`
		}
		return filepath.Join(home, ".bashrc"), `export PATH="$HOME/.local/bin:$PATH"`
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish"), "fish_add_path $HOME/.local/bin"
	}
	return filepath.Join(home, ".profile"), `export PATH="$HOME/.local/bin:$PATH"`
}

const winUserPath = `[Environment]::GetEnvironmentVariable('Path','User')`

// pathProfile names what AddCLIToPath edits and whether it already did.
func pathProfile() (string, bool) {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("powershell", "-NoProfile", "-Command", winUserPath).Output()
		if err != nil {
			return "user PATH", false
		}
		for _, p := range strings.Split(strings.TrimSpace(string(out)), ";") {
			if strings.EqualFold(filepath.Clean(p), filepath.Clean(cliInstallDir())) {
				return "user PATH", true
			}
		}
		return "user PATH", false
	}
	file, _ := shellProfile()
	b, _ := os.ReadFile(file)
	return file, strings.Contains(string(b), pathMarker)
}

// AddCLIToPath puts the install directory on PATH for new terminals: a line
// in the login shell's profile, or the user PATH on Windows. Running it
// twice changes nothing.
func (m *MoleService) AddCLIToPath() error {
	if _, done := pathProfile(); done {
		return nil
	}
	if runtime.GOOS == "windows" {
		script := `$d=$env:MOLE_BIN_DIR; $p=` + winUserPath + `; [Environment]::SetEnvironmentVariable('Path',($p.TrimEnd(';')+';'+$d),'User')`
		cmd := exec.Command("powershell", "-NoProfile", "-Command", script)
		cmd.Env = append(os.Environ(), "MOLE_BIN_DIR="+cliInstallDir())
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("update user PATH: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	file, line := shellProfile()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n%s\n%s\n", pathMarker, line)
	return err
}
