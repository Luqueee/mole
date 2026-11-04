package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Luqueee/mole/internal/config"
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

// isolateState points config.StateDir at a fresh directory (XDG_STATE_HOME
// on Unix, LOCALAPPDATA on Windows), and the home and config directories
// the login service lives in, so tests never touch a real daemon or service.
func isolateState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	return config.StateDir()
}

func TestClaimInstance_LockAndRelease(t *testing.T) {
	isolateState(t)
	inst, err := claimInstance(context.Background(), false)
	if err != nil {
		t.Fatalf("claimInstance: %v", err)
	}
	if pid, ok := readPid(); !ok || pid != os.Getpid() {
		t.Errorf("pidfile = %d, %v; want our pid %d", pid, ok, os.Getpid())
	}
	if held, err := lockHeld(); err != nil || !held {
		t.Errorf("lockHeld = %v, %v; want true", held, err)
	}
	if d := findDaemon(); !d.running || d.pid != os.Getpid() {
		t.Errorf("findDaemon = %+v; want running with our pid", d)
	}
	// The pidfile keeps its one-line format: the desktop app reads it
	// whole with strings.TrimSpace.
	if data, _ := os.ReadFile(pidPath()); string(data) != strconv.Itoa(os.Getpid())+"\n" {
		t.Errorf("pidfile content = %q", data)
	}

	if _, err := claimInstance(context.Background(), false); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("already running (pid %d)", os.Getpid())) {
		t.Errorf("second claim: err = %v; want already running", err)
	}

	inst.release()
	if held, err := lockHeld(); err != nil || held {
		t.Errorf("after release: lockHeld = %v, %v; want false", held, err)
	}
	if _, err := os.Stat(pidPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after release: pidfile still present (%v)", err)
	}
	if d := findDaemon(); d.running || d.stalePid != 0 {
		t.Errorf("after release: findDaemon = %+v; want nothing", d)
	}
}

// A claim made under the service manager waits for the running mole
// instead of failing, and gives up when its context ends.
func TestClaimInstance_WaitStopsOnCancel(t *testing.T) {
	isolateState(t)
	inst, err := claimInstance(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.release()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var claimErr error
	out := captureBoth(t, func() { _, claimErr = claimInstance(ctx, true) })
	if !errors.Is(claimErr, context.DeadlineExceeded) {
		t.Errorf("waiting claim: err = %v; want the context's error", claimErr)
	}
	if !strings.Contains(out, "waiting for it to stop") {
		t.Errorf("waiting claim did not say it waits:\n%s", out)
	}
}

// A pidfile left by a lock-aware mole whose lock is free is stale, whatever
// process holds that PID today.
func TestFindDaemon_StaleAfterLockAwareExit(t *testing.T) {
	dir := isolateState(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getppid() // alive, and not ours to signal
	if err := os.WriteFile(lockPath(), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePid(pid); err != nil {
		t.Fatal(err)
	}
	if d := findDaemon(); d.running || d.stalePid != pid {
		t.Errorf("findDaemon = %+v; want stale pid %d", d, pid)
	}
}

// The PID-reuse case: a stale pidfile naming a live, unrelated process.
// `mole down` must clean up without signalling it.
func TestRunDown_NeverSignalsAnUnrelatedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep(1)")
	}
	isolateState(t)
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = other.Wait(); close(exited) }()
	t.Cleanup(func() { _ = other.Process.Kill(); <-exited })
	if err := writePid(other.Process.Pid); err != nil {
		t.Fatal(err)
	}

	var code int
	out := captureBoth(t, func() { code = runDown(nil) })
	if code != 1 || !strings.Contains(out, "stale pidfile") {
		t.Errorf("runDown = %d, output:\n%s\nwant 1 and a stale-pidfile note", code, out)
	}
	select {
	case <-exited:
		t.Fatal("runDown signalled an unrelated process")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(pidPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale pidfile not removed (%v)", err)
	}
}

func TestExeName(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/mole":           "mole",
		"/usr/local/bin/mole (deleted)": "mole",
		`C:\Tools\Mole.EXE`:             "mole",
		"mole.test":                     "mole.test",
		"/usr/bin/sleep":                "sleep",
		"":                              "",
	} {
		if got := exeName(in); got != want {
			t.Errorf("exeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// daemonHelperEnv selects what TestDaemonHelperProcess does when this test
// binary is re-executed as a stand-in mole server.
const daemonHelperEnv = "MOLE_TEST_DAEMON_HELPER"

// TestDaemonHelperProcess is not a test: it is the body of the helper
// process. "server" behaves like a lock-aware mole up (claims the
// instance, shuts down slowly when stopped); "legacy" like a mole from
// before the lock (holds nothing, exits when stopped).
func TestDaemonHelperProcess(t *testing.T) {
	mode := os.Getenv(daemonHelperEnv)
	if mode == "" {
		return
	}
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := stopContext(sigCtx, stopSignals)
	defer cancel()
	var inst *instance
	if mode == "server" {
		var err error
		if inst, err = claimInstance(ctx, false); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Minute):
		os.Exit(4)
	}
	time.Sleep(300 * time.Millisecond) // a shutdown that takes a moment
	inst.release()
	os.Exit(0)
}

// startDaemonHelper runs TestDaemonHelperProcess in mode and reaps it.
func startDaemonHelper(t *testing.T, mode string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonHelperProcess$")
	cmd.Env = append(os.Environ(), daemonHelperEnv+"="+mode)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	return cmd, exited
}

func TestRunDown_WaitsForExit(t *testing.T) {
	isolateState(t)
	cmd, exited := startDaemonHelper(t, "server")
	pid := cmd.Process.Pid
	deadline := time.Now().Add(10 * time.Second)
	for d := findDaemon(); !d.running || d.pid != pid; d = findDaemon() {
		if time.Now().After(deadline) {
			t.Fatalf("helper never claimed the instance (findDaemon = %+v)", d)
		}
		time.Sleep(20 * time.Millisecond)
	}

	start := time.Now()
	var code int
	out := captureBoth(t, func() { code = runDown(nil) })
	if code != 0 || !strings.Contains(out, fmt.Sprintf("mole stopped (pid %d)", pid)) {
		t.Fatalf("runDown = %d, output:\n%s", code, out)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("runDown returned after %v, before the server finished shutting down", elapsed)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("helper still running after runDown reported it stopped")
	}
	if _, err := os.Stat(pidPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pidfile still present after down (%v)", err)
	}
	if d := findDaemon(); d.running {
		t.Errorf("findDaemon after down = %+v", d)
	}
}

// A server started by a mole from before the instance lock is still found
// (its executable is mole) and stopped.
func TestRunDown_StopsPreLockServer(t *testing.T) {
	isolateState(t)
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd, exited := startDaemonHelper(t, "legacy")
	pid := cmd.Process.Pid
	if err := writePid(pid); err != nil {
		t.Fatal(err)
	}
	// Let the helper install its signal handler.
	time.Sleep(300 * time.Millisecond)
	if d := findDaemon(); !d.running || d.pid != pid {
		t.Fatalf("findDaemon = %+v; want the pre-lock server %d", d, pid)
	}
	var code int
	out := captureBoth(t, func() { code = runDown(nil) })
	if code != 0 {
		t.Fatalf("runDown = %d, output:\n%s", code, out)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("pre-lock server still running after down")
	}
	if _, err := os.Stat(pidPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pidfile still present after down (%v)", err)
	}
}

func TestRunDown_NotRunning(t *testing.T) {
	isolateState(t)
	var code int
	out := captureBoth(t, func() { code = runDown(nil) })
	if code != 1 || !strings.Contains(out, "not running (no pidfile found)") {
		t.Errorf("runDown = %d, output:\n%s", code, out)
	}
}
