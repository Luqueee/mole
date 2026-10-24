//go:build windows

package main

import (
	"context"

	"github.com/Luqueee/mole/internal/tunnel"
)

// watchResize does nothing on Windows: the remote terminal keeps the size it
// started with.
func watchResize(context.Context, int) <-chan tunnel.Size { return nil }
