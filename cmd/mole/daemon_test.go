package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunRestart_NoRemote exercises the early-exit path when the
// resolved config has no `remote:` set. The function must refuse
// to restart without that information — it has no way of knowing
// what target to reconnect to.
func TestRunRestart_NoRemote(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mole.yaml")
	if err := os.WriteFile(cfgPath, []byte("auto_discover: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var code int
	out := captureBoth(t, func() {
		code = runRestart([]string{"-config", cfgPath})
	})
	if code != 1 {
		t.Errorf("runRestart with no remote: code = %d, want 1", code)
	}
	if !strings.Contains(out, "no remote configured") {
		t.Errorf("output missing 'no remote configured' hint:\n%s", out)
	}
}

// TestRunRestart_ConfigMissing covers the case where -config points
// at a path that doesn't exist. config.Load() falls back to defaults
// silently, so a missing file is not itself an error — but with no
// remote in the defaults the same "no remote configured" path fires.
// This is fine: it's the same defensive exit.
func TestRunRestart_ConfigMissing(t *testing.T) {
	var code int
	out := captureBoth(t, func() {
		code = runRestart([]string{"-config", "/does/not/exist/mole.yaml"})
	})
	if code != 1 {
		t.Errorf("runRestart with missing config: code = %d, want 1", code)
	}
	if !strings.Contains(out, "no remote configured") {
		t.Errorf("output missing 'no remote configured' hint:\n%s", out)
	}
}

// TestRunRestart_BadConfigFlag exercises the path where -config's
// value is a file that doesn't parse as YAML. The Load call
// returns an error and we surface it via stderr.
func TestRunRestart_BadConfigFlag(t *testing.T) {
	// Point at a file that exists but isn't valid YAML.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mole.yaml")
	if err := os.WriteFile(cfgPath, []byte("not yaml: : :\tbroken: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	var code int
	_ = captureBoth(t, func() {
		code = runRestart([]string{"-config", cfgPath})
	})
	if code != 1 {
		t.Errorf("runRestart with broken config: code = %d, want 1", code)
	}
}

// captureBoth redirects both stdout and stderr to a pipe for the
// duration of fn and returns the combined output. Used for tests
// that need to assert against either stream (e.g. error-path
// messages that go to stderr).
func captureBoth(t *testing.T, fn func()) string {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	os.Stderr = w
	defer func() {
		os.Stdout = origOut
		os.Stderr = origErr
	}()
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	fn()
	_ = w.Close()
	<-done
	return buf.String()
}
