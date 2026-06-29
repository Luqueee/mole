//go:build windows

package main

import (
	"os"
	"syscall"
)

// Windows process creation flags (from <winbase.h>).
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

// detachSysProcAttr starts the child detached from the console and in a
// new process group so it keeps running after the parent exits.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
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
