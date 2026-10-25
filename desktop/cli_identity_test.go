package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Luqueee/mole/internal/brew"
)

// fakeBin writes an executable shell script named mole into dir that prints
// out for any arguments, and returns its path.
func fakeBin(t *testing.T, dir, out string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script binaries")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "mole")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+out+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", home)
	brewDirs = nil
	t.Cleanup(func() { brewDirs = brew.Prefixes })
	return home
}

// Another program called mole comes first on PATH: it must be skipped, not run.
func TestMoleBinSkipsAnotherProgramNamedMole(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	other := fakeBin(t, filepath.Join(root, "other"), "Mole version 1.44.1")
	ours := fakeBin(t, filepath.Join(root, "ours"), "mole v0.7.0 (v0.7.0)")
	t.Setenv("PATH", filepath.Dir(other)+string(os.PathListSeparator)+filepath.Dir(ours))

	got, err := moleBin()
	if err != nil || got != ours {
		t.Fatalf("moleBin = %q, %v; want %q", got, err, ours)
	}
}

func TestMoleBinNotFoundWhenOnlyAnotherProgramExists(t *testing.T) {
	isolate(t)
	other := fakeBin(t, t.TempDir(), "Mole version 1.44.1")
	t.Setenv("PATH", filepath.Dir(other))
	if got, err := moleBin(); err == nil {
		t.Fatalf("moleBin = %q, want an error: the only mole on PATH is another program", got)
	}
}

// An app opened from the Finder has no PATH; Homebrew's prefix is still searched.
func TestMoleBinFindsHomebrewPrefixWithoutPATH(t *testing.T) {
	isolate(t)
	t.Setenv("PATH", "")
	prefix := t.TempDir()
	ours := fakeBin(t, prefix, "mole v0.7.0")
	brewDirs = []string{prefix}
	if got, err := moleBin(); err != nil || got != ours {
		t.Fatalf("moleBin = %q, %v; want %q", got, err, ours)
	}
}

// The verdict is cached per file, but a replaced file is judged again.
func TestIsMoleRechecksAChangedFile(t *testing.T) {
	isolate(t)
	p := fakeBin(t, t.TempDir(), "Mole version 1.44.1")
	if isMole(p) {
		t.Fatal("another program accepted")
	}
	fakeBin(t, filepath.Dir(p), "mole v0.7.0 and a longer line so the size differs")
	if !isMole(p) {
		t.Fatal("replaced file still judged by the old verdict")
	}
}

func TestMoleIdentity(t *testing.T) {
	for out, want := range map[string]bool{
		"mole v0.7.0":                  true,
		"mole v0.7.0 (v0.7.0)":         true,
		"\nmole v10.12.3 (main@abc)\n": true,
		"Mole version 1.44.1":          false,
		"mole version 1.0":             false,
		"v0.7.0":                       false,
		"":                             false,
	} {
		if got := moleIdentity.MatchString(out); got != want {
			t.Errorf("moleIdentity(%q) = %v, want %v", out, got, want)
		}
	}
}

// A brew-managed CLI is left alone: no second copy that would shadow it.
func TestInstallCLIRefusesWhenHomebrewOwnsTheBinary(t *testing.T) {
	if bundledCLI() == nil {
		t.Skip("CLI not embedded")
	}
	isolate(t)
	keg := filepath.Join(t.TempDir(), "Cellar", "mole-tunnel", "0.7.0", "bin")
	fakeBin(t, keg, "mole v0.7.0")
	link := t.TempDir()
	if err := os.Symlink(filepath.Join(keg, "mole"), filepath.Join(link, "mole")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", link)

	m := NewMoleService()
	if st := m.GetCLI(); !st.Installed || st.ManagedBy != "mole-tunnel" {
		t.Fatalf("GetCLI = %+v, want installed and managed by mole-tunnel", st)
	}
	err := m.InstallCLI()
	if err == nil || !strings.Contains(err.Error(), "brew upgrade mole-tunnel") {
		t.Fatalf("InstallCLI = %v, want a pointer to brew upgrade", err)
	}
	if _, statErr := os.Stat(filepath.Join(cliInstallDir(), cliName())); statErr == nil {
		t.Error("InstallCLI wrote a second copy next to Homebrew's")
	}
}
