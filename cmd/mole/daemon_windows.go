//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
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

// processAlive reports whether a process with the given PID is running. A
// process that has exited but whose handle someone still holds is not.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// It exists, but belongs to someone we may not inspect.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// gracefulStopWait is how long terminate lets a server shut down cleanly
// after a stop request before it kills it.
const gracefulStopWait = 3 * time.Second

// terminate stops the process. Windows has no SIGTERM: a mole server
// watches a named event (see watchStopRequests) and shuts down cleanly when
// it is set. A server that does not watch one (an older mole) or does not
// exit in time is killed, as before.
func terminate(pid int) error {
	if requestStop(pid) == nil && waitExit(pid, gracefulStopWait) {
		return nil
	}
	if !processAlive(pid) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// stopEventName is the per-process event `mole down` sets to stop pid. The
// Local\ namespace is per logon session.
func stopEventName(pid int) string { return `Local\mole-stop-` + strconv.Itoa(pid) }

// requestStop sets pid's stop event.
func requestStop(pid int) error {
	name, err := windows.UTF16PtrFromString(stopEventName(pid))
	if err != nil {
		return err
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.SetEvent(h)
}

// watchStopRequests creates this process's stop event and calls stop when
// `mole down` sets it. Without the event, `mole down` falls back to Kill.
func watchStopRequests(stop context.CancelFunc) {
	name, err := windows.UTF16PtrFromString(stopEventName(os.Getpid()))
	if err != nil {
		return
	}
	h, err := windows.CreateEvent(nil, 1, 0, name)
	if h == 0 || (err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS)) {
		return
	}
	// The handle stays open for the life of the process.
	go func() {
		ev, err := windows.WaitForSingleObject(h, windows.INFINITE)
		if err == nil && ev == windows.WAIT_OBJECT_0 {
			stop()
		}
	}()
}

// processImage returns the full path of the executable pid runs.
func processImage(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}

// lockRegion is the byte range LockFileEx locks: far past the PID the lock
// file holds, so other processes can still read it (Windows byte-range
// locks are mandatory).
const (
	lockRegionOffsetHigh = 0x40000000
	lockRegionLen        = 1
)

// lockFile takes an exclusive, non-blocking LockFileEx lock on f. Windows
// releases it when the handle closes, including on process exit.
func lockFile(f *os.File) error {
	ol := &windows.Overlapped{OffsetHigh: lockRegionOffsetHigh}
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, lockRegionLen, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errLocked
	}
	return err
}

func unlockFile(f *os.File) error {
	ol := &windows.Overlapped{OffsetHigh: lockRegionOffsetHigh}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockRegionLen, 0, ol)
}
