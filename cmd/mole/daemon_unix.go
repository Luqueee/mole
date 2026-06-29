//go:build !windows

package main

import (
	"os"
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
