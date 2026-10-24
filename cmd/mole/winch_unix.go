//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/Luqueee/mole/internal/tunnel"
)

// watchResize reports changes of the terminal's size until ctx ends.
func watchResize(ctx context.Context, fd int) <-chan tunnel.Size {
	out := make(chan tunnel.Size, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				if w, h, err := term.GetSize(fd); err == nil {
					select {
					case out <- tunnel.Size{Rows: h, Cols: w}:
					default:
					}
				}
			}
		}
	}()
	return out
}
