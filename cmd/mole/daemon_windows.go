//go:build windows

package main

import (
	"os"
	"syscall"
)

// Windows process creation flags (from <winbase.h>).
const (
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
)

// detachSysProcAttr starts the child outside the console and process group.
// Breaking away from the SSH server's job also lets it survive logout.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob}
}

// processAlive reports whether a process with the given PID exists.
// On Windows os.FindProcess returns an error when the process is gone.
func processAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}

// terminate stops the process. Windows has no SIGTERM, so kill it.
func terminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
