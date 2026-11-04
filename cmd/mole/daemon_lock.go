// Single-instance lock and process identity for the mole server.
//
// The pidfile alone cannot tell a running mole from an unrelated process
// that inherited its PID after a crash or a reboot, so the running server
// also holds an exclusive OS lock on mole.lock in the state directory for
// its whole lifetime. The OS drops the lock when the process dies, however
// it dies, which makes "the lock is held" a liveness test that PID reuse
// cannot fool. The lock file also records the holder's PID; the pidfile
// keeps its historical one-line format for the desktop app.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Luqueee/mole/internal/config"
)

func lockPath() string { return filepath.Join(config.StateDir(), "mole.lock") }

// errLocked reports that another process holds the instance lock.
var errLocked = errors.New("instance lock is held by another process")

// errLockUnsupported reports a platform without file locking; liveness then
// falls back to the process identity check alone.
var errLockUnsupported = errors.New("file locking is not supported on this platform")

// instance is the running server's claim on the state directory.
type instance struct {
	lock *os.File // nil when locking is unsupported
	pid  int
}

// lockAttempts and lockRetryDelay let a server start ride out the instant
// during which `mole down` or `mole status`-style probes hold the lock.
const (
	lockAttempts   = 10
	lockRetryDelay = 20 * time.Millisecond
)

// acquireInstanceLock takes the instance lock and records our PID in it.
func acquireInstanceLock() (*os.File, error) {
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	for i := 0; ; i++ {
		err = lockFile(f)
		if err == nil || !errors.Is(err, errLocked) || i == lockAttempts-1 {
			break
		}
		time.Sleep(lockRetryDelay)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return f, nil
}

// lockHeld reports whether some process holds the instance lock. It probes
// by trying to take the lock and releasing it at once.
func lockHeld() (bool, error) {
	f, err := os.OpenFile(lockPath(), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	switch err := lockFile(f); {
	case err == nil:
		_ = unlockFile(f)
		return false, nil
	case errors.Is(err, errLocked):
		return true, nil
	default:
		return false, err
	}
}

// readLockPid returns the PID recorded by the last holder of the lock.
func readLockPid() int {
	data, err := os.ReadFile(lockPath())
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(string(data), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// daemonInfo describes the mole server recorded in the state directory.
type daemonInfo struct {
	running  bool
	pid      int // PID that is safe to signal; 0 when unknown
	stalePid int // pidfile PID that does not belong to a running mole
}

// findDaemon reports the running mole server, if any. A PID is only ever
// reported as running when it is known to be a mole server: either the
// instance lock is held and the PID is its holder, or (for a server started
// by a mole that predates the lock) the process's executable is mole.
func findDaemon() daemonInfo {
	filePid, hasFile := readPid()
	held, lockErr := lockHeld()
	if lockErr == nil && held {
		pid := readLockPid()
		if pid == 0 && hasFile {
			pid = filePid
		}
		if pid != 0 && (!processAlive(pid) || !mayBeMole(pid)) {
			pid = 0
		}
		return daemonInfo{running: true, pid: pid}
	}
	if !hasFile {
		return daemonInfo{}
	}
	// The pidfile was written by a lock-aware mole whose lock is now free:
	// that server is gone, whatever process has the PID today.
	if lockErr == nil && readLockPid() == filePid {
		return daemonInfo{stalePid: filePid}
	}
	if filePid != os.Getpid() && processAlive(filePid) {
		if isMole, known := isMoleProcess(filePid); known && isMole {
			return daemonInfo{running: true, pid: filePid}
		}
	}
	return daemonInfo{stalePid: filePid}
}

// mayBeMole is false only when pid is known not to be a mole executable.
func mayBeMole(pid int) bool {
	isMole, known := isMoleProcess(pid)
	return !known || isMole
}

// isMoleProcess reports whether pid runs a mole executable. known is false
// when the executable cannot be determined.
func isMoleProcess(pid int) (isMole, known bool) {
	image, err := processImage(pid)
	if err != nil || strings.TrimSpace(image) == "" {
		return false, false
	}
	own := ""
	if exe, err := os.Executable(); err == nil {
		own = exeName(exe)
	}
	name := exeName(image)
	return strings.HasPrefix(name, "mole") || (own != "" && name == own), true
}

// exeName normalises an executable path to a comparable base name: no
// directory, no Windows .exe suffix, no Linux " (deleted)" marker left by a
// binary replaced on disk (e.g. by `mole update`).
func exeName(path string) string {
	path = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(path), " (deleted)"))
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		path = path[i+1:]
	}
	name := strings.ToLower(path)
	return strings.TrimSuffix(name, ".exe")
}

// alreadyRunningMessage is the refusal printed when a server is running.
func alreadyRunningMessage(pid int) string {
	return "mole is already running" + pidSuffix(pid) + ". Use 'mole down' to stop it."
}

func pidSuffix(pid int) string {
	if pid == 0 {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", pid)
}

// claimInstance makes this process the user's one mole server: it takes the
// instance lock and writes the pidfile. When wait is set (a service manager
// started us) it waits for a running server to stop instead of failing, so
// the manager does not restart mole in a loop; ctx ends the wait.
func claimInstance(ctx context.Context, wait bool) (*instance, error) {
	waiting := false
	for {
		inst, running, err := tryClaimInstance()
		if err != nil || inst != nil {
			return inst, err
		}
		if !wait {
			return nil, errors.New(alreadyRunningMessage(running.pid))
		}
		if !waiting {
			waiting = true
			fmt.Fprintf(os.Stderr, "mole is already running%s; waiting for it to stop before starting\n", pidSuffix(running.pid))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// tryClaimInstance makes one attempt; it returns the running server's info
// when another mole holds the claim.
func tryClaimInstance() (*instance, daemonInfo, error) {
	// This also catches a server started by a mole that predates the lock:
	// it holds no lock and is recognised by the pidfile and its executable.
	if d := findDaemon(); d.running && d.pid != os.Getpid() {
		return nil, d, nil
	}
	lock, err := acquireInstanceLock()
	switch {
	case errors.Is(err, errLocked):
		return nil, findDaemon(), nil
	case errors.Is(err, errLockUnsupported):
		lock = nil
	case err != nil:
		return nil, daemonInfo{}, fmt.Errorf("error: cannot claim the mole state directory: %w", err)
	}
	inst := &instance{lock: lock, pid: os.Getpid()}
	if err := writePid(inst.pid); err != nil {
		fmt.Fprintln(os.Stderr, "warn: could not write pidfile:", err)
	}
	return inst, daemonInfo{}, nil
}

// release removes our pidfile and drops the lock. The lock file itself
// stays: deleting it would let two servers lock two different files.
func (in *instance) release() {
	if in == nil {
		return
	}
	removePidFile(in.pid)
	if in.lock != nil {
		_ = unlockFile(in.lock)
		_ = in.lock.Close()
	}
}

// removePidFile deletes the pidfile if it still names pid, so a server
// that has taken over in the meantime keeps its own.
func removePidFile(pid int) {
	if cur, ok := readPid(); ok && cur == pid {
		_ = os.Remove(pidPath())
	}
}
