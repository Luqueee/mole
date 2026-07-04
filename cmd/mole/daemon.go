// Background (daemon) support for `mole up -d` and `mole down`.
//
// Go can't fork after the runtime starts, so daemonization is done by
// re-executing the same binary detached from the controlling terminal
// (see detachSysProcAttr in daemon_unix.go / daemon_windows.go). The
// parent writes a pidfile and returns; the child runs the normal
// foreground server with stdio redirected to a log file.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

// runRestart stops a backgrounded mole (if any) and re-launches it
// in the background using the same config the user was running
// before. The flow is:
//
//  1. Load the active config (same resolution as `mole up`: explicit
//     -config, then ./mole.yaml, then user-global).
//  2. If there's no remote in the config, refuse — we don't know
//     what to reconnect to. Tell the user to run `mole init` or
//     `mole up -remote X` first.
//  3. If a daemon is running, runDown-equivalent: signal the PID
//     from the pidfile and wait for it to exit.
//  4. daemonize with the same config + remote.
//
// This is the operation the user reaches for when they want
// "the live ports I just added to take effect" without re-typing
// -remote. It's intentionally a thin wrapper: a future change
// (e.g. a config-reload endpoint) would obviate it.
func runRestart(args []string) int {
	fs := flag.NewFlagSet("restart", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Load config to find the remote.
	resolved := *configPath
	if resolved == "" {
		resolved = config.Find()
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if cfg.Remote == "" {
		fmt.Fprintln(os.Stderr, "no remote configured; run `mole init` or `mole up -remote <name>` first")
		return 1
	}

	// Stop the running daemon if any. We use the same primitive as
	// `mole down`: read pidfile, signal, remove. If no daemon is
	// running, print a note and proceed to start one (so `mole
	// restart` doubles as `mole up -d` when nothing is up yet).
	if pid, ok := readPid(); ok {
		if !processAlive(pid) {
			fmt.Fprintf(os.Stderr, "stale pidfile (pid %d not running); cleaning up\n", pid)
			_ = os.Remove(pidPath())
		} else {
			if err := terminate(pid); err != nil {
				fmt.Fprintf(os.Stderr, "could not stop pid %d: %v\n", pid, err)
				return 1
			}
			// Wait for the process to actually exit. SIGTERM gives
			// it a moment; up to ~2s is plenty for a Go program
			// tearing down SSH tunnels and HTTP listeners.
			for i := 0; i < 20; i++ {
				if !processAlive(pid) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if processAlive(pid) {
				fmt.Fprintf(os.Stderr, "pid %d did not exit within 2s; refusing to start a second instance\n", pid)
				return 1
			}
		}
	} else {
		fmt.Fprintln(os.Stderr, "note: no mole up running; starting one")
	}

	// Re-launch. Pass -config and -remote so the new daemon reads
	// the same config file (which may have been edited by `mole
	// ports add` etc.) and connects to the same target. daemonize
	// handles -d internally; we don't pass it here.
	upArgs := []string{"-remote", cfg.Remote}
	if resolved != "" {
		upArgs = append(upArgs, "-config", resolved)
	}
	return daemonize(upArgs)
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
