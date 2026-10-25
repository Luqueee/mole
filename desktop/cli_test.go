package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Luqueee/mole/internal/brew"
)

func TestInstallCLI(t *testing.T) {
	want := bundledCLI()
	if want == nil {
		t.Skip("CLI not embedded; run `wails3 task bundle:cli`")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", home)
	t.Setenv("PATH", "")
	brewDirs = nil // a mole of this machine's Homebrew must not count
	t.Cleanup(func() { brewDirs = brew.Prefixes })
	m := NewMoleService()

	if st := m.GetCLI(); st.Installed || !st.Bundled {
		t.Fatalf("before install: %+v", st)
	}
	if err := m.InstallCLI(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(cliInstallDir(), cliName()))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("installed binary differs: %v", err)
	}
	// Found without the install dir being on PATH, as for a Finder launch.
	st := m.GetCLI()
	if !st.Installed || !st.UpToDate || st.DirOnPath || st.Version == "" {
		t.Fatalf("after install: %+v", st)
	}
	// Reinstalling over an existing copy works.
	if err := m.InstallCLI(); err != nil {
		t.Fatal(err)
	}
	// A stale copy is reported, then fixed.
	p := filepath.Join(cliInstallDir(), cliName())
	if err := os.WriteFile(p, append(want[:len(want):len(want)], 0), 0o755); err != nil {
		t.Fatal(err)
	}
	if m.GetCLI().UpToDate {
		t.Fatal("modified binary reported up to date")
	}
	if err := m.InstallCLI(); err != nil || !m.GetCLI().UpToDate {
		t.Fatalf("update failed: %v", err)
	}
}

func TestAddCLIToPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("edits the real user PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	m := NewMoleService()

	if st := m.GetCLI(); st.PathSet || st.Profile != filepath.Join(home, ".zshrc") {
		t.Fatalf("before: %+v", st)
	}
	for range 2 { // the second call must not add a second line
		if err := m.AddCLIToPath(); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(home, ".zshrc"))
	if strings.Count(string(b), pathMarker) != 1 || !strings.Contains(string(b), `$HOME/.local/bin`) {
		t.Fatalf(".zshrc: %q", b)
	}
	if !m.GetCLI().PathSet {
		t.Fatal("PathSet false after adding")
	}
}
