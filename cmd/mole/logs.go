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
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func runLogs(args []string) int {
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
