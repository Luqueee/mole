// Background (daemon) support for `mole up -d` and `mole down`.
//
// Go can't fork after the runtime starts, so daemonization is done by
// re-executing the same binary detached from the controlling terminal
// (see detachSysProcAttr in daemon_unix.go / daemon_windows.go). The
// parent writes a pidfile and returns; the child runs the normal
// foreground server with stdio redirected to a log file.
package main

import (
	"context"
	"errors"
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

	// Refuse to start a second instance if one is already running. The
	// child re-checks under the instance lock, which settles races.
	if d := findDaemon(); d.running {
		fmt.Fprintln(os.Stderr, alreadyRunningMessage(d.pid))
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

	logStart := int64(0)
	if st, err := lf.Stat(); err == nil {
		logStart = st.Size()
	}
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
	// The child writes the pidfile itself once it holds the instance lock
	// (see claimInstance), so a child that loses a start race never
	// overwrites the running server's pidfile.
	pid := cmd.Process.Pid
	// A child that dies at once (no remote configured, bad config, a port in
	// use) must not be reported as started: wait a moment and say why.
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
		if held, err := lockHeld(); err == nil && !held {
			removePidFile(pid) // it died before it could clean up
		}
		fmt.Fprintln(os.Stderr, "error: mole exited right after starting:")
		fmt.Fprint(os.Stderr, newLogLines(logPath(), logStart))
		return 1
	case <-time.After(startupGrace):
	}

	fmt.Printf("mole started in background (pid %d)\n", pid)
	if strings.Contains(newLogLines(logPath(), logStart), connectRetryMessage) {
		fmt.Println("  note:   the remote is not reachable yet; mole keeps retrying (see 'mole logs')")
	}
	fmt.Printf("  logs:   %s\n", logPath())
	fmt.Printf("  status: mole status\n")
	fmt.Printf("  stop:   mole down\n")
	return 0
}

// startupGrace is how long a freshly started daemon must stay alive to count
// as started.
const startupGrace = 1500 * time.Millisecond

// newLogLines returns what was appended to the log after offset, last lines
// only, indented, for an error message.
func newLogLines(path string, offset int64) string {
	b, err := os.ReadFile(path)
	if err != nil || int64(len(b)) <= offset {
		return "  (nothing was logged)\n"
	}
	lines := strings.Split(strings.TrimRight(string(b[offset:]), "\n"), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return "  " + strings.Join(lines, "\n  ") + "\n"
}

// stopTimeout bounds how long `mole down` and `mole restart` wait for the
// server to exit after asking it to stop.
const stopTimeout = 5 * time.Second

// errStillRunning reports a server that outlived stopTimeout.
var errStillRunning = fmt.Errorf("still running %s after the stop request", stopTimeout)

// stopDaemon asks pid to shut down and waits for it to exit.
func stopDaemon(pid int) error {
	if err := terminate(pid); err != nil {
		return err
	}
	if !waitExit(pid, stopTimeout) {
		return errStillRunning
	}
	return nil
}

// waitExit polls until pid has exited or timeout elapses and reports
// whether it exited.
func waitExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// stopContext extends sigCtx, the run's SIGINT/SIGTERM context, with the
// platform's other stop request (`mole down` on Windows, which has no
// SIGTERM). After the first signal it restores the default handling, so a
// second Ctrl+C ends a slow shutdown at once.
func stopContext(sigCtx context.Context, stopSignals context.CancelFunc) (context.Context, context.CancelFunc) {
	go func() {
		<-sigCtx.Done()
		stopSignals()
	}()
	ctx, cancel := context.WithCancel(sigCtx)
	watchStopRequests(cancel)
	return ctx, cancel
}

// runDown stops the running mole (backgrounded, or in the foreground of
// another terminal or a service manager), waits for it to exit and removes
// its pidfile. Only a process known to be the mole server is signalled.
func runDown(_ []string) int {
	d := findDaemon()
	if !d.running {
		if d.stalePid != 0 {
			fmt.Fprintf(os.Stderr, "mole is not running (stale pidfile for pid %d); cleaning up\n", d.stalePid)
			removePidFile(d.stalePid)
		} else {
			fmt.Fprintln(os.Stderr, "mole is not running (no pidfile found)")
		}
		return 1
	}
	if d.pid == 0 {
		fmt.Fprintf(os.Stderr, "mole is running, but its pid is unknown (%s is missing or stale); stop it where it was started\n", pidPath())
		return 1
	}
	if err := stopDaemon(d.pid); err != nil {
		if errors.Is(err, errStillRunning) {
			fmt.Fprintf(os.Stderr, "error: mole (pid %d) is %v; see 'mole logs'\n", d.pid, err)
		} else {
			fmt.Fprintf(os.Stderr, "error: could not stop mole (pid %d): %v\n", d.pid, err)
		}
		return 1
	}
	removePidFile(d.pid)
	fmt.Printf("mole stopped (pid %d)\n", d.pid)
	printServiceNoteAfterDown(d.pid)
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
//     from the pidfile and wait for it to exit. A daemon run by the
//     login service (`mole service install`) is restarted through the
//     service manager instead, so it stays supervised.
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
	// `mole down`: find the server, signal it, wait for it to exit. If no
	// daemon is running, print a note and proceed to start one (so `mole
	// restart` doubles as `mole up -d` when nothing is up yet).
	d := findDaemon()
	switch {
	case d.running && d.pid == 0:
		fmt.Fprintf(os.Stderr, "mole is running, but its pid is unknown (%s is missing or stale); stop it where it was started\n", pidPath())
		return 1
	case d.running:
		// A server the login service runs is restarted by the service
		// manager, so it stays supervised.
		if handled, code := restartManagedService(d.pid); handled {
			return code
		}
		if err := stopDaemon(d.pid); err != nil {
			fmt.Fprintf(os.Stderr, "could not stop pid %d: %v; refusing to start a second instance\n", d.pid, err)
			return 1
		}
		removePidFile(d.pid)
	case d.stalePid != 0:
		fmt.Fprintf(os.Stderr, "stale pidfile (pid %d not running); cleaning up\n", d.stalePid)
		removePidFile(d.stalePid)
	default:
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
