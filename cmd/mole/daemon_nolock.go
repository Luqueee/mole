//go:build !windows && !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package main

import "os"

// Platforms without flock(2) in package syscall (e.g. Solaris, AIX) fall
// back to the pidfile plus the process identity check.
func lockFile(*os.File) error   { return errLockUnsupported }
func unlockFile(*os.File) error { return nil }
