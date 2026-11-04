// mole get — download files from another machine over SSH.
//
//	mole get [-o DIR] [-no-verify] NODE:PATH...
//
// The other machine needs nothing of mole: only a POSIX shell, tail and,
// to verify the download, sha256sum (or shasum / openssl). An interrupted
// download keeps what arrived, and running the same command again continues
// from there.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/transfer"
	"github.com/Luqueee/mole/internal/tunnel"
)

// splitTarget splits NODE:PATH at the first colon.
func splitTarget(arg string) (node, remotePath string, err error) {
	node, remotePath, ok := strings.Cut(arg, ":")
	if !ok || node == "" || remotePath == "" {
		return "", "", fmt.Errorf("%q is not NODE:PATH (for example devlabs:/var/log/syslog)", arg)
	}
	if strings.HasPrefix(node, "-") || strings.ContainsAny(node, "\x00\n\r") {
		return "", "", fmt.Errorf("invalid node %q", node)
	}
	return node, remotePath, nil
}

// remotePathPrelude sets f to the file named by $1, expanding a leading ~ the
// way a shell would (the path reaches the script as data, never as code).
const remotePathPrelude = `case "$1" in "~") f=$HOME ;; "~/"*) f=$HOME/${1#"~/"} ;; *) f=$1 ;; esac
`

const getStatScript = remotePathPrelude + `[ -e "$f" ] || { echo "no such file: $1" >&2; exit 3; }
[ -d "$f" ] && { echo "$1 is a directory (only files can be fetched)" >&2; exit 4; }
[ -f "$f" ] || { echo "$1 is not a regular file" >&2; exit 4; }
[ -r "$f" ] || { echo "permission denied: $1" >&2; exit 5; }
wc -c < "$f"
m=$(stat -c %Y "$f" 2>/dev/null || stat -f %m "$f" 2>/dev/null); echo "${m:-0}"
m=$(stat -c %a "$f" 2>/dev/null || stat -f %Lp "$f" 2>/dev/null); echo "${m:-644}"
`

const getOpenScript = remotePathPrelude + `if [ "$2" -eq 0 ]; then exec cat -- "$f"; else exec tail -c "+$(($2 + 1))" -- "$f"; fi
`

// exit status 9 means this machine has no way to compute a SHA-256.
const (
	getNoSumStatus = 9
	getSumScript   = remotePathPrelude + `if command -v sha256sum >/dev/null 2>&1; then exec sha256sum -- "$f"
elif command -v shasum >/dev/null 2>&1; then exec shasum -a 256 -- "$f"
elif command -v openssl >/dev/null 2>&1; then exec openssl dgst -sha256 -r "$f"
else exit 9; fi
`
)

// sshSource reads files from a machine through SSH sessions.
type sshSource struct{ mgr *tunnel.Manager }

func shellCommand(script string, args ...string) string {
	var b strings.Builder
	b.WriteString(remoteEnv + "sh -c " + shellQuote(script) + " sh")
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String()
}

func (s sshSource) run(ctx context.Context, stdout io.Writer, script string, args ...string) (code int, stderr string, err error) {
	var errb bytes.Buffer
	code, err = s.mgr.Exec(ctx, shellCommand(script, args...), tunnel.ExecOptions{Stdout: stdout, Stderr: &errb})
	return code, strings.TrimSpace(errb.String()), err
}

func (s sshSource) Stat(ctx context.Context, p string) (transfer.FileInfo, error) {
	var out bytes.Buffer
	code, msg, err := s.run(ctx, &out, getStatScript, p)
	if err != nil {
		return transfer.FileInfo{}, err
	}
	if code != 0 {
		if msg == "" {
			msg = fmt.Sprintf("cannot read %s (exit status %d)", p, code)
		}
		return transfer.FileInfo{}, errors.New(msg)
	}
	f := strings.Fields(out.String())
	if len(f) != 3 {
		return transfer.FileInfo{}, fmt.Errorf("unexpected answer from the remote: %q", out.String())
	}
	size, err1 := strconv.ParseInt(f[0], 10, 64)
	mtime, err2 := strconv.ParseInt(f[1], 10, 64)
	mode, err3 := strconv.ParseUint(f[2], 8, 32)
	if err := errors.Join(err1, err2, err3); err != nil {
		return transfer.FileInfo{}, fmt.Errorf("unexpected answer from the remote: %q", out.String())
	}
	name := path.Base(strings.TrimRight(p, "/"))
	if name == "/" || name == "." || name == "~" {
		return transfer.FileInfo{}, fmt.Errorf("%s does not name a file", p)
	}
	return transfer.FileInfo{Name: name, Size: size, Mode: uint32(mode), MTime: mtime}, nil
}

// pipeReader ends the remote command when the reader is closed.
type pipeReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (p pipeReader) Close() error {
	p.cancel()
	return p.PipeReader.Close()
}

func (s sshSource) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	go func() {
		code, msg, err := s.run(ctx, pw, getOpenScript, p, strconv.FormatInt(offset, 10))
		switch {
		case err != nil:
			_ = pw.CloseWithError(err)
		case code != 0:
			if msg == "" {
				msg = fmt.Sprintf("exit status %d", code)
			}
			_ = pw.CloseWithError(fmt.Errorf("reading %s on the remote failed: %s", p, msg))
		default:
			_ = pw.Close()
		}
	}()
	return pipeReader{pr, cancel}, nil
}

func (s sshSource) Sum(ctx context.Context, p string) (string, bool, error) {
	var out bytes.Buffer
	code, msg, err := s.run(ctx, &out, getSumScript, p)
	if err != nil {
		return "", false, err
	}
	switch {
	case code == getNoSumStatus:
		return "", false, nil
	case code != 0:
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", code)
		}
		return "", false, errors.New(msg)
	}
	f := strings.Fields(out.String())
	if len(f) == 0 || len(f[0]) != 64 {
		return "", false, fmt.Errorf("unexpected checksum answer from the remote: %q", out.String())
	}
	return f[0], true, nil
}

// getConns keeps one SSH connection per machine for the whole command.
type getConns struct {
	cfg *config.Config
	log *slog.Logger
	by  map[string]*sshSource
}

func (c *getConns) source(name string) (*sshSource, error) {
	if s, ok := c.by[name]; ok {
		return s, nil
	}
	node, err := lookupNode(c.cfg, name)
	registered := err == nil
	if !registered {
		node = config.NodeConfig{Remote: name} // any SSH host name or user@host
	}
	mgr, err := dialNode(c.cfg, node, c.log)
	if err != nil {
		if !registered {
			return nil, fmt.Errorf("cannot reach %s: %w (it is not a registered node, so it was tried as an SSH host)", name, err)
		}
		return nil, fmt.Errorf("cannot reach %s: %w", name, err)
	}
	s := &sshSource{mgr: mgr}
	c.by[name] = s
	return s, nil
}

func (c *getConns) close() {
	for _, s := range c.by {
		_ = s.mgr.Close()
	}
}

func runGet(args []string) int {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	dir := fs.String("o", ".", "folder to save the files into")
	noVerify := fs.Bool("no-verify", false, "do not compare the download with the checksum computed on the other machine")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole get [flags] NODE:PATH [NODE:PATH...]

Downloads files from another machine over SSH. NODE is a registered node
(mole node ls), the config's own remote, or any SSH host. The other machine
needs only a POSIX shell and tail, plus sha256sum (or shasum, openssl) to
verify the download. A leading ~/ in PATH is that machine's home folder.

If a download is interrupted, what arrived is kept: run the same command
again to continue from there.

Flags:
  -o DIR       folder to save the files into (default: the current folder)
  -no-verify   skip the checksum comparison (it reads the file a second time there)
  -config      path to YAML config`)
	}
	targets, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(targets) == 0 {
		fs.Usage()
		return 2
	}
	type target struct{ node, path string }
	var list []target
	for _, a := range targets {
		node, p, err := splitTarget(a)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mole get:", err)
			return 2
		}
		list = append(list, target{node, p})
	}
	cfg, err := loadNodeConfigAt(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	conns := &getConns{cfg: cfg, log: newLogger("warn"), by: map[string]*sshSource{}}
	defer conns.close()

	failed := false
	for _, t := range list {
		label := t.node + ":" + t.path
		src, err := conns.source(t.node)
		if err != nil {
			printFailed(label, err)
			failed = true
			continue
		}
		ui := newProgress(path.Base(strings.TrimRight(t.path, "/")))
		first := true
		start := time.Now()
		res, err := transfer.Pull(ctx, src, t.path, *dir, transfer.PullOptions{
			NoVerify: *noVerify,
			Peer:     t.node,
			Progress: func(got, size int64) {
				if first { // bytes kept from an earlier attempt do not count towards the speed
					ui.base, first = got, false
				}
				ui.update(got, size)
			},
		})
		ui.clear()
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "mole get: interrupted")
				if res.Size >= 1<<20 {
					fmt.Fprintln(os.Stderr, "what arrived is kept: run the same command again to continue")
				}
				return 130
			}
			printFailed(label, err)
			if res.Size >= 1<<20 {
				fmt.Fprintln(os.Stderr, "  run the same command again to retry or continue")
			}
			failed = true
			continue
		}
		printGot(label, res, filepath.Join(*dir, res.Saved), time.Since(start), *noVerify)
	}
	if failed {
		return 1
	}
	return 0
}

func printGot(label string, res transfer.PullResult, dest string, took time.Duration, noVerify bool) {
	color := cliColor(os.Stdout)
	line := fmt.Sprintf("%s %s  %s  %s %s  %s", cGreen("✓", color), label, cDim(humanSize(res.Size), color), cDim("→", color), dest, cDim(took.Round(time.Millisecond).String(), color))
	if res.Resumed > 0 {
		line += "  " + cDim("(continued from "+humanSize(res.Resumed)+")", color)
	}
	fmt.Println(line)
	if !res.Verified && !noVerify {
		fmt.Fprintf(os.Stderr, "  %s\n", cDim("not verified: the other machine has no sha256sum, shasum or openssl (the size matches)", cliColor(os.Stderr)))
	}
}
