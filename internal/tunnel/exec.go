package tunnel

import (
	"context"
	"errors"
	"io"
	"time"

	"golang.org/x/crypto/ssh"
)

// Size is a terminal size in character cells.
type Size struct{ Rows, Cols int }

// PTYOptions asks for a terminal on the remote.
type PTYOptions struct {
	Term string // e.g. "xterm-256color"
	Size Size
}

// ExecOptions describes the stdio of a remote command. All fields are
// optional; a nil Stdin means the command sees end of input at once.
type ExecOptions struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// PTY allocates a terminal for the command (stdout and stderr then
	// arrive merged on Stdout, and Ctrl+C written to Stdin interrupts it).
	PTY *PTYOptions
	// Resize delivers terminal size changes while the command runs.
	Resize <-chan Size
}

// interruptGrace is how long a command gets to stop after SIGINT before its
// session is closed.
const interruptGrace = 3 * time.Second

// Exec runs cmd on the remote and returns its exit status. A command that
// ends without one (killed by a signal, connection lost) returns -1 and an
// error. Cancelling ctx interrupts the command and, if it does not stop,
// closes the session.
func (m *Manager) Exec(ctx context.Context, cmd string, o ExecOptions) (int, error) {
	sess, err := m.newSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()

	if o.PTY != nil {
		modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
		if err := sess.RequestPty(o.PTY.Term, o.PTY.Size.Rows, o.PTY.Size.Cols, modes); err != nil {
			return -1, err
		}
	}
	sess.Stdout, sess.Stderr = o.Stdout, o.Stderr
	var stdin io.WriteCloser
	if o.Stdin != nil {
		// Not sess.Stdin: Wait would block on a reader (a terminal) that
		// never ends. The copy below is simply abandoned when the command is
		// done.
		if stdin, err = sess.StdinPipe(); err != nil {
			return -1, err
		}
	}
	if err := sess.Start(cmd); err != nil {
		return -1, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(stdin, o.Stdin)
			_ = stdin.Close()
		}()
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	for {
		select {
		case err := <-done:
			return exitStatus(err)
		case sz := <-o.Resize:
			_ = sess.WindowChange(sz.Rows, sz.Cols)
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGINT)
			select {
			case err := <-done:
				code, _ := exitStatus(err)
				return code, ctx.Err()
			case <-time.After(interruptGrace):
				_ = sess.Close()
				return -1, ctx.Err()
			}
		}
	}
}

func exitStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return exit.ExitStatus(), nil
	}
	return -1, err
}
