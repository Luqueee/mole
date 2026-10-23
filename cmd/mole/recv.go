// mole recv — the receiving end of `mole send`, run on the remote by the
// local daemon over SSH. It is plumbing, not something to type by hand.
//
//	mole recv --stdio       serve one transfer on stdin/stdout (SSH is the auth)
//	mole recv --set-token   store the shared token and peer port read from stdin
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/transfer"
)

// stdioConn presents stdin and stdout as one connection.
type stdioConn struct {
	io.Reader
	io.Writer
}

func runRecv(args []string) int {
	fs := flag.NewFlagSet("recv", flag.ExitOnError)
	stdio := fs.Bool("stdio", false, "serve one transfer on stdin/stdout")
	setToken := fs.Bool("set-token", false, "store the token and peer port read from stdin")
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole recv --stdio | --set-token

Plumbing for 'mole send': the local mole daemon runs this on the remote over
SSH. Use 'mole send' and 'mole inbox' instead.`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch {
	case *setToken:
		if err := recvSetToken(os.Stdin, config.StateDir()); err != nil {
			fmt.Fprintln(os.Stderr, "mole recv:", err)
			return 1
		}
		return 0
	case *stdio:
		cfg, err := config.Load(resolveConfigPathReadOnly(*configPath))
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := recvStdio(ctx, cfg, stdioConn{os.Stdin, os.Stdout}); err != nil {
			fmt.Fprintln(os.Stderr, "mole recv:", err)
			return 1
		}
		return 0
	}
	fs.Usage()
	return 2
}

// recvStdio serves one transfer on rw. Nothing but protocol may be written to
// it; diagnostics go to stderr.
func recvStdio(ctx context.Context, cfg *config.Config, rw io.ReadWriter) error {
	rcv := &transfer.Receiver{Inbox: cfg.Transfer.InboxDir(), Peer: "peer", MaxBytes: cfg.Transfer.MaxBytes()}
	return rcv.Handle(ctx, rw)
}

// recvSetToken stores "token\nport\n" read from r under stateDir. The port is
// optional.
func recvSetToken(r io.Reader, stateDir string) error {
	b, err := io.ReadAll(io.LimitReader(r, 512))
	if err != nil {
		return err
	}
	lines := strings.Fields(string(b))
	if len(lines) == 0 || !transfer.ValidToken(lines[0]) {
		return fmt.Errorf("invalid token")
	}
	if err := transfer.WriteToken(transfer.TokenPath(stateDir), lines[0]); err != nil {
		return err
	}
	if len(lines) > 1 {
		port, err := strconv.Atoi(lines[1])
		if err != nil {
			return fmt.Errorf("invalid port %q", lines[1])
		}
		if err := transfer.WritePeerPort(transfer.PeerPortPath(stateDir), port); err != nil {
			return err
		}
	}
	return nil
}
