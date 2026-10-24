// mole -n NODE <command> — run a mole command on another node.
//
//	mole -n devlabs ports add 3000
//	mole -n devlabs status
//	mole -n devlabs send            # the picker runs on devlabs, in your terminal
//
// The command runs there exactly as if typed on that machine (its config, its
// daemon, its inbox). Nodes are registered with `mole node add`; the
// config's own `remote:` also works as a node name.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/tunnel"
)

// shellQuote quotes s as one word for a POSIX shell (fish reads it the same).
func shellQuote(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteCommand is the command line that runs `bin args...` on a node, with
// the PATH a login would give (a non-interactive SSH command gets a minimal one).
func remoteCommand(bin string, args ...string) string {
	var b strings.Builder
	b.WriteString(remoteEnv)
	if rest, ok := strings.CutPrefix(bin, "~/"); ok {
		b.WriteString(`"$HOME"/` + shellQuote(rest))
	} else {
		b.WriteString(shellQuote(bin))
	}
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String()
}

// splitNodeFlag recognises a leading `-n NAME` / `--node NAME` (or `=NAME`)
// and returns the node and the rest of the arguments.
func splitNodeFlag(args []string) (node string, rest []string, found bool, err error) {
	if len(args) == 0 {
		return "", nil, false, nil
	}
	a := args[0]
	switch {
	case a == "-n" || a == "--node" || a == "-node":
		if len(args) < 2 {
			return "", nil, true, errors.New("-n needs a node name")
		}
		return args[1], args[2:], true, nil
	case strings.HasPrefix(a, "-n=") || strings.HasPrefix(a, "--node=") || strings.HasPrefix(a, "-node="):
		_, v, _ := strings.Cut(a, "=")
		return v, args[1:], true, nil
	}
	return "", nil, false, nil
}

// loadNodeConfig loads the config that holds the node registry.
func loadNodeConfig() (*config.Config, error) {
	return loadNodeConfigAt("")
}

func loadNodeConfigAt(path string) (*config.Config, error) {
	return config.Load(resolveConfigPathReadOnly(path))
}

// lookupNode resolves name or explains what the choices are.
func lookupNode(cfg *config.Config, name string) (config.NodeConfig, error) {
	if n, ok := cfg.Node(name); ok {
		return n, nil
	}
	known := cfg.NodeNames()
	if len(known) == 0 {
		return config.NodeConfig{}, fmt.Errorf("unknown node %q: none registered yet (mole node add NAME TARGET)", name)
	}
	return config.NodeConfig{}, fmt.Errorf("unknown node %q: known nodes are %s", name, strings.Join(known, ", "))
}

// dialNode opens the SSH connection to a node.
func dialNode(cfg *config.Config, n config.NodeConfig, log *slog.Logger) (*tunnel.Manager, error) {
	port := n.SSHPort
	if port == 0 {
		port = cfg.SSHPort
	}
	rem, err := tunnel.ResolveRemote(n.Remote, port)
	if err != nil {
		return nil, err
	}
	return tunnel.New(rem, n.Insecure, log)
}

func runOnNode(name string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: mole -n NODE <command> [args...]\n\nRuns a mole command on another node. Register nodes with `mole node add`.")
		return 2
	}
	cfg, err := loadNodeConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	node, err := lookupNode(cfg, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole:", err)
		return 1
	}
	mgr, err := dialNode(cfg, node, newLogger("warn"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mole: cannot reach node %s: %v\n", name, err)
		return 255
	}
	defer mgr.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stdinFd, stdoutFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	opts := tunnel.ExecOptions{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	switch {
	case term.IsTerminal(stdinFd) && term.IsTerminal(stdoutFd):
		// A terminal on both ends: give the command a real one (pickers,
		// colours, Ctrl+C reaching it), and put ours in raw mode meanwhile.
		w, h, err := term.GetSize(stdoutFd)
		if err != nil {
			w, h = 80, 24
		}
		termName := os.Getenv("TERM")
		if termName == "" {
			termName = "xterm-256color"
		}
		opts.PTY = &tunnel.PTYOptions{Term: termName, Size: tunnel.Size{Rows: h, Cols: w}}
		opts.Stderr = nil // a terminal has one output
		opts.Resize = watchResize(ctx, stdoutFd)
		if old, err := term.MakeRaw(stdinFd); err == nil {
			defer term.Restore(stdinFd, old)
		}
	case term.IsTerminal(stdinFd):
		opts.Stdin = nil // output is being piped: do not wait for the keyboard
	}

	code, err := mgr.Exec(ctx, remoteCommand(node.BinOrDefault(), args...), opts)
	if err != nil {
		if opts.PTY != nil {
			fmt.Fprint(os.Stderr, "\r\n")
		}
		fmt.Fprintf(os.Stderr, "mole: node %s: %v\n", name, err)
		if code < 0 {
			return 255
		}
	}
	return code
}
