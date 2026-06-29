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
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole logs [flags]

Show the background daemon log with colourised formatting.

Flags:
  -f          follow the log (like tail -f)
  -n <num>    trailing lines to show (default 200)
  -raw        print raw lines, no formatting
  -color      force colour even when piped
  -no-color   disable colour`)
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

	color := *forceColor || (!*noColor && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout))
	if *noColor {
		color = false
	}
	render := func(line string) {
		if line == "" {
			return
		}
		if *raw {
			fmt.Println(line)
			return
		}
		fmt.Println(formatLine(line, color))
	}

	// Print the last n lines.
	tail, err := lastLines(f, *lines)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: reading log:", err)
		return 1
	}
	for _, l := range tail {
		render(l)
	}

	if !*follow {
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
				render(strings.TrimRight(pending, "\n"))
				pending = ""
			}
		}
		if err == io.EOF {
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
