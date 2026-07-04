// `mole logs` — pretty, colorized viewer for the background daemon log.
//
// The detached server (mole up -d) writes slog text lines to
// StateDir()/mole.log. This command parses those logfmt lines and
// renders them with level badges, a dimmed timestamp, a bright message
// and faint key=value attributes. Colour is disabled automatically when
// stdout isn't a TTY or NO_COLOR is set.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runLogs dispatches on the first positional arg:
//
//	mole logs        tail the last N lines of the daemon log (default)
//	mole logs -f     follow mode
//	mole logs clean  truncate or rotate the log
//
// We dispatch on the first arg because the existing CLI parses
// flags with flag.Parse first and a positional subcommand is
// easier to extend than parsing -follow/-clean as a sub-mode of
// the -f flag.
func runLogs(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "clean":
			return runLogsClean(args[1:])
		}
	}
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
 	var (
		follow     = fs.Bool("f", false, "follow the log (stream new lines)")
		lines      = fs.Int("n", 200, "number of trailing lines to show")
		raw        = fs.Bool("raw", false, "print raw log lines without formatting")
		noColor    = fs.Bool("no-color", false, "disable colour output")
		forceColor = fs.Bool("color", false, "force colour even when stdout isn't a TTY")
		noDedup    = fs.Bool("no-dedup", false, "don't collapse repeated lines into one (×N)")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole logs [flags]

Show the background daemon log with colourised formatting.

Flags:
  -f          follow the log (like tail -f)
  -n <num>    trailing lines to show (default 200)
  -raw        print raw lines, no formatting
  -color      force colour even when piped
  -no-color   disable colour
  -no-dedup   don't collapse repeated lines into one (×N)`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path := logPath()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "no logs yet at %s\nstart mole in the background with 'mole up -d'.\n", path)
			return 1
		}
		fmt.Fprintln(os.Stderr, "error: cannot open log:", err)
		return 1
	}
	defer f.Close()

	tty := isTerminal(os.Stdout)
	color := *forceColor || (!*noColor && os.Getenv("NO_COLOR") == "" && tty)
	if *noColor {
		color = false
	}
	// Live in-place collapse only on a real TTY in follow mode: it uses
	// carriage returns, which would corrupt a pipe or file.
	live := *follow && tty && !*raw && !*noDedup
	col := &collapser{color: color, raw: *raw, dedup: !*noDedup, live: live}

	// Print the last n lines.
	tail, err := lastLines(f, *lines)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: reading log:", err)
		return 1
	}
	for _, l := range tail {
		col.emit(l)
	}

	if !*follow {
		col.flush()
		return 0
	}

	// Follow: poll for appended lines. Seek to current end and stream.
	off, _ := f.Seek(0, io.SeekEnd)
	reader := bufio.NewReader(f)
	var pending string
	for {
		chunk, err := reader.ReadString('\n')
		if chunk != "" {
			pending += chunk
			if strings.HasSuffix(pending, "\n") {
				col.emit(strings.TrimRight(pending, "\n"))
				pending = ""
			}
		}
		if err == io.EOF {
			// Nothing more right now. In buffered mode, flush the pending
			// group so its count is visible without waiting for a
			// differing line. In live mode, idle is a no-op: the run line
			// stays open and keeps updating in place as repeats arrive.
			col.idle()
			// Detect truncation/rotation: if the file shrank, reopen.
			if fi, statErr := os.Stat(path); statErr == nil && fi.Size() < off {
				if nf, oErr := os.Open(path); oErr == nil {
					f.Close()
					f = nf
					reader = bufio.NewReader(f)
					off = 0
					continue
				}
			}
			off, _ = f.Seek(0, io.SeekCurrent)
			time.Sleep(400 * time.Millisecond)
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: following log:", err)
			return 1
		}
	}
}

// lastLines returns the last n non-empty-trimmed lines of r. The daemon
// log is small, so reading it whole is fine.
func lastLines(r io.Reader, n int) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var all []string
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// runLogsClean truncates the daemon log. By default it deletes the
// file's contents (size 0); with -keep N it preserves the last N
// non-empty lines and rewrites the file with just those. We rewrite
// rather than truncate-and-rewrite-append so concurrent writers
// (the running daemon) see a clean cut instead of a half-finished
// file mid-rewrite.
//
// If the log doesn't exist, this is a no-op — we don't create an
// empty log just because the user asked to clean it.
func runLogsClean(args []string) int {
	fs := flag.NewFlagSet("logs clean", flag.ExitOnError)
	keep := fs.Int("keep", 0, "keep the last N lines instead of truncating to zero")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole logs clean [-keep N]

Truncates the daemon log. With -keep N, preserves the last N
non-empty lines and rewrites the file with just those. Without
-keep, the log is truncated to 0 bytes.

Flags:
  -keep N   keep the last N lines (default 0: truncate)`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *keep < 0 {
		fmt.Fprintln(os.Stderr, "mole logs clean: -keep must be >= 0")
		return 2
	}

	path := logPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "mole logs clean: no log file at", path)
			return 0
		}
		fmt.Fprintln(os.Stderr, "mole logs clean: read", path, ":", err)
		return 1
	}

	var keepLines []string
	if *keep > 0 {
		keepLines, err = lastLines(bytes.NewReader(data), *keep)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mole logs clean: parse:", err)
			return 1
		}
	}

	// Atomic write: write to a tmp file in the same directory and
	// rename over the original. POSIX rename is atomic within a
	// directory, so concurrent readers (the running daemon tailing
	// itself, or `mole logs -f`) see either the old or the new
	// file, never a truncated-mid-write mess.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mole-log-clean-*.tmp")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole logs clean: tmp file:", err)
		return 1
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup if the rename never happens.
		_ = os.Remove(tmpName)
	}()
	if len(keepLines) > 0 {
		// Each kept line gets a trailing newline; the original
		// file was line-oriented (slog writes one record per
		// line) so we restore that.
		for _, l := range keepLines {
			if _, err := tmp.WriteString(l + "\n"); err != nil {
				_ = tmp.Close()
				fmt.Fprintln(os.Stderr, "mole logs clean: write:", err)
				return 1
			}
		}
	}
	if err := tmp.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "mole logs clean: close tmp:", err)
		return 1
	}
	if err := os.Rename(tmpName, path); err != nil {
		fmt.Fprintln(os.Stderr, "mole logs clean: rename:", err)
		return 1
	}

	if *keep > 0 {
		fmt.Printf("mole logs clean: kept last %d lines in %s\n", len(keepLines), path)
	} else {
		fmt.Printf("mole logs clean: truncated %s\n", path)
	}
	return 0
}
