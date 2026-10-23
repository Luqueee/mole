package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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

// moleBin finds the CLI the app shells out to. Apps started from the Finder
// or the Start menu don't inherit the shell's PATH, so the install directory
// is tried too.
func moleBin() (string, error) {
	if p, err := exec.LookPath(cliName()); err == nil {
		return p, nil
	}
	p := filepath.Join(cliInstallDir(), cliName())
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("mole CLI not found: install it from the Setup tab")
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
	bin, err := moleBin()
	if err != nil {
		return st
	}
	st.Installed, st.Path = true, bin
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
