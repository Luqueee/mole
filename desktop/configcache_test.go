package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// useConfigFile points the app at a scratch global config and returns a
// writer for it. Unless fresh is set, each write is backdated a minute: the
// file looks like one that has not changed for a while.
func useConfigFile(t *testing.T) func(body string, fresh bool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Chdir(t.TempDir()) // no ./mole.yaml to shadow the global config
	path := filepath.Join(dir, "mole", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	return func(body string, fresh bool) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if !fresh {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestLoadConfigKeepsTheParseUntilTheFileChanges(t *testing.T) {
	write := useConfigFile(t)
	write("remote: aaaa\n", false)
	a, _, err := loadConfig()
	if err != nil || a.Remote != "aaaa" {
		t.Fatalf("first load: %+v, %v", a, err)
	}
	if b, _, _ := loadConfig(); b != a {
		t.Fatal("an unchanged file was parsed again")
	}

	write("remote: bbbbbb\n", false) // new size, same mtime
	if b, _, _ := loadConfig(); b.Remote != "bbbbbb" {
		t.Fatalf("a change of size was missed: remote = %q", b.Remote)
	}

	// Same size and mtime: only an invalidation (what the app does after
	// running the CLI) reveals the change.
	write("remote: cccccc\n", false)
	invalidateConfig()
	if c, _, _ := loadConfig(); c.Remote != "cccccc" {
		t.Fatalf("an invalidated cache was used: remote = %q", c.Remote)
	}
}

func TestLoadConfigSeesANewModTime(t *testing.T) {
	write := useConfigFile(t)
	write("remote: aaaa\n", false)
	if a, _, _ := loadConfig(); a.Remote != "aaaa" {
		t.Fatalf("remote = %q", a.Remote)
	}
	write("remote: bbbb\n", true) // same size, now
	if b, _, _ := loadConfig(); b.Remote != "bbbb" {
		t.Fatalf("a new mtime was missed: remote = %q", b.Remote)
	}
}

// A file written a moment ago may be written again within its mtime's
// granularity without its stamp changing, so its parse is not reused.
func TestLoadConfigDoesNotKeepAFreshFile(t *testing.T) {
	write := useConfigFile(t)
	write("remote: aaaa\n", true)
	a, _, _ := loadConfig()
	if b, _, _ := loadConfig(); a == b {
		t.Fatal("a file written within the racy window was cached")
	}
}

func TestLoadConfigErrorsAreNotCached(t *testing.T) {
	write := useConfigFile(t)
	write("remote: [unclosed\n", false)
	if _, _, err := loadConfig(); err == nil {
		t.Fatal("a broken config loaded")
	}
	write("remote: fixed\n", true)
	if c, _, err := loadConfig(); err != nil || c.Remote != "fixed" {
		t.Fatalf("after the fix: %+v, %v", c, err)
	}
}
