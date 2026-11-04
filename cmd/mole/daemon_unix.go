//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// detachSysProcAttr starts the child in its own session (setsid) so it
// survives the parent shell closing and has no controlling terminal.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// processAlive reports whether a process with the given PID exists.
// On Unix, signal 0 performs the existence/permission check without
// actually delivering a signal.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// terminate asks the process to shut down gracefully (SIGTERM), which
// mole's signal handler turns into a clean shutdown.
func terminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// processImage returns the executable (path or name) pid runs: the
// /proc/<pid>/exe link on Linux, the ps(1) command name elsewhere.
func processImage(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		dir := "/proc/" + strconv.Itoa(pid)
		if exe, err := os.Readlink(dir + "/exe"); err == nil {
			return exe, nil
		}
		comm, err := os.ReadFile(dir + "/comm")
		return strings.TrimSpace(string(comm)), err
	}
	ps := "/bin/ps"
	if _, err := os.Stat(ps); err != nil {
		ps = "ps"
	}
	args := []string{"-o", "comm=", "-p", strconv.Itoa(pid)}
	switch runtime.GOOS {
	case "darwin", "dragonfly", "freebsd", "netbsd", "openbsd":
		args = append([]string{"-ww"}, args...) // never truncate a long path
	}
	out, err := exec.Command(ps, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// watchStopRequests is a no-op on Unix: `mole down` sends SIGTERM, which
// the signal context already observes.
func watchStopRequests(context.CancelFunc) {}
