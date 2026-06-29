// Background (daemon) support for `mole up -d` and `mole down`.
//
// Go can't fork after the runtime starts, so daemonization is done by
// re-executing the same binary detached from the controlling terminal
// (see detachSysProcAttr in daemon_unix.go / daemon_windows.go). The
// parent writes a pidfile and returns; the child runs the normal
// foreground server with stdio redirected to a log file.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Luqueee/mole/internal/config"
)

func pidPath() string { return filepath.Join(config.StateDir(), "mole.pid") }
func logPath() string { return filepath.Join(config.StateDir(), "mole.log") }

// daemonize re-execs the current binary with the same arguments (minus
// the detach flag) in a new session, redirecting stdio to the log file
// and recording the child's PID. It returns a process exit code.
func daemonize(upArgs []string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot locate own binary to daemonize:", err)
		return 1
	}

	// Refuse to start a second instance if one is already running.
	if pid, ok := readPid(); ok && processAlive(pid) {
		fmt.Fprintf(os.Stderr, "mole is already running (pid %d). Use 'mole down' to stop it.\n", pid)
		return 1
	}

	dir := config.StateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot create state dir:", err)
		return 1
	}
	lf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot open log file:", err)
		return 1
	}
	defer lf.Close()

	cmd := exec.Command(exe, append([]string{"up"}, upArgs...)...)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	// MOLE_DAEMONIZED tells the child it is the detached server, so it
	// runs the forwarder instead of recursing into daemonize again.
	cmd.Env = append(os.Environ(), "MOLE_DAEMONIZED=1")
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "error: failed to start background process:", err)
		return 1
	}
	// Capture the PID before Release(), which resets Process.Pid to -1.
	pid := cmd.Process.Pid
	if err := writePid(pid); err != nil {
		fmt.Fprintln(os.Stderr, "warn: could not write pidfile:", err)
	}
	// Detach: don't wait on the child.
	_ = cmd.Process.Release()

	fmt.Printf("mole started in background (pid %d)\n", pid)
	fmt.Printf("  logs:   %s\n", logPath())
	fmt.Printf("  status: mole status\n")
	fmt.Printf("  stop:   mole down\n")
	return 0
}

// runDown stops a backgrounded mole by signalling the PID in the
// pidfile and removing it.
func runDown(_ []string) int {
	pid, ok := readPid()
	if !ok {
		fmt.Fprintln(os.Stderr, "mole is not running (no pidfile found)")
		return 1
	}
	if !processAlive(pid) {
		fmt.Fprintf(os.Stderr, "mole is not running (stale pidfile for pid %d); cleaning up\n", pid)
		_ = os.Remove(pidPath())
		return 1
	}
	if err := terminate(pid); err != nil {
		fmt.Fprintf(os.Stderr, "error: could not stop mole (pid %d): %v\n", pid, err)
		return 1
	}
	_ = os.Remove(pidPath())
	fmt.Printf("mole stopped (pid %d)\n", pid)
	return 0
}

func readPid() (int, bool) {
	data, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func writePid(pid int) error {
	return os.WriteFile(pidPath(), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// stripDetachFlags removes the background flags from an argument slice
// so the re-exec'd child runs in the foreground. Both -d and -detach
// (and their double-dash forms) are recognised; they take no value.
func stripDetachFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "-d", "--d", "-detach", "--detach":
			continue
		}
		out = append(out, a)
	}
	return out
}
