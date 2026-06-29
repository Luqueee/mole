package main

import (
	"fmt"
	"strings"
	"time"
)

// Truecolor (24-bit) ANSI helpers. Modern terminals support these; we
// only emit them when the caller decided colour is on.
func fg(r, g, b int, s string) string {
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", r, g, b, s)
}

func badge(fr, fgc, fb, br, bg, bb int, s string) string {
	return fmt.Sprintf("\x1b[1;38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm%s\x1b[0m", fr, fgc, fb, br, bg, bb, s)
}

// levelStyle returns a coloured, fixed-width badge for a log level.
// Colours follow a modern dark-UI palette (GitHub-ish).
func levelStyle(level string, color bool) string {
	label := fmt.Sprintf(" %-5s ", level)
	if !color {
		return "[" + strings.TrimSpace(level) + "]"
	}
	switch strings.ToUpper(level) {
	case "ERROR":
		return badge(255, 255, 255, 248, 81, 73, label) // white on red
	case "WARN":
		return badge(20, 20, 20, 210, 153, 34, label) // black on amber
	case "INFO":
		return badge(255, 255, 255, 56, 139, 253, label) // white on blue
	case "DEBUG":
		return badge(230, 237, 243, 87, 96, 106, label) // light on gray
	default:
		return badge(230, 237, 243, 87, 96, 106, label)
	}
}

// collapser folds consecutive identical log lines (ignoring their
// timestamp) into a single rendered line with a "(×N)" repeat counter.
// It buffers the current run and prints on flush — when a differing
// line arrives, at end of input, or on a follow idle tick.
type collapser struct {
	color bool
	raw   bool
	dedup bool

	key   string // identity of the current run (line minus timestamp)
	first string // rendered text of the run's first line
	count int
}

func (c *collapser) emit(line string) {
	if line == "" {
		return
	}
	if !c.dedup {
		fmt.Println(c.render(line))
		return
	}
	k := collapseKey(line)
	if c.count > 0 && k == c.key {
		c.count++
		return
	}
	c.flush()
	c.key = k
	c.first = c.render(line)
	c.count = 1
}

func (c *collapser) flush() {
	if c.count == 0 {
		return
	}
	out := c.first
	if c.count > 1 {
		out += countSuffix(c.count, c.color)
	}
	fmt.Println(out)
	c.count = 0
}

func (c *collapser) render(line string) string {
	if c.raw {
		return line
	}
	return formatLine(line, c.color)
}

// collapseKey returns a line's identity for dedupe purposes: everything
// except the timestamp, so the same event at different times collapses.
func collapseKey(line string) string {
	pairs := parseLogfmt(line)
	if len(pairs) == 0 {
		return line
	}
	var b strings.Builder
	for _, kv := range pairs {
		if kv[0] == "time" {
			continue
		}
		b.WriteString(kv[0])
		b.WriteByte('=')
		b.WriteString(kv[1])
		b.WriteByte('\x00')
	}
	return b.String()
}

func countSuffix(n int, color bool) string {
	s := fmt.Sprintf(" (×%d)", n)
	if color {
		return fg(110, 114, 125, s)
	}
	return s
}

// formatLine turns one slog text line into a pretty, aligned, coloured
// line. Unrecognised lines (not logfmt) are returned unchanged.
func formatLine(line string, color bool) string {
	pairs := parseLogfmt(line)
	if len(pairs) == 0 {
		return line
	}

	var ts, level, msg string
	var attrs [][2]string
	for _, kv := range pairs {
		switch kv[0] {
		case "time":
			ts = kv[1]
		case "level":
			level = kv[1]
		case "msg":
			msg = kv[1]
		default:
			attrs = append(attrs, kv)
		}
	}
	// Not a slog line we understand — leave it be.
	if level == "" && msg == "" {
		return line
	}

	var b strings.Builder

	// Timestamp → HH:MM:SS, dimmed.
	if ts != "" {
		short := ts
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			short = t.Format("15:04:05")
		} else if t, err := time.Parse("2006-01-02T15:04:05.000Z07:00", ts); err == nil {
			short = t.Format("15:04:05")
		}
		if color {
			b.WriteString(fg(110, 114, 125, short))
		} else {
			b.WriteString(short)
		}
		b.WriteByte(' ')
	}

	// Level badge.
	if level != "" {
		b.WriteString(levelStyle(level, color))
		b.WriteByte(' ')
	}

	// Message, bright/bold.
	if msg != "" {
		if color {
			b.WriteString("\x1b[1m" + fg(230, 237, 243, msg))
		} else {
			b.WriteString(msg)
		}
	}

	// Attributes: faint key, tinted value.
	for _, kv := range attrs {
		b.WriteByte(' ')
		if color {
			b.WriteString(fg(125, 133, 144, kv[0]+"="))
			b.WriteString(fg(173, 186, 199, kv[1]))
		} else {
			b.WriteString(kv[0] + "=" + kv[1])
		}
	}

	return b.String()
}

// parseLogfmt parses a slog text line ("k=v k2=\"v 2\" ...") into ordered
// key/value pairs, honouring double-quoted values with backslash
// escapes.
func parseLogfmt(line string) [][2]string {
	var out [][2]string
	i, n := 0, len(line)
	for i < n {
		for i < n && line[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}
		start := i
		for i < n && line[i] != '=' && line[i] != ' ' {
			i++
		}
		key := line[start:i]
		val := ""
		if i < n && line[i] == '=' {
			i++ // consume '='
			if i < n && line[i] == '"' {
				i++ // opening quote
				var sb strings.Builder
				for i < n && line[i] != '"' {
					if line[i] == '\\' && i+1 < n {
						i++
					}
					sb.WriteByte(line[i])
					i++
				}
				if i < n {
					i++ // closing quote
				}
				val = sb.String()
			} else {
				vs := i
				for i < n && line[i] != ' ' {
					i++
				}
				val = line[vs:i]
			}
		}
		if key != "" {
			out = append(out, [2]string{key, val})
		}
	}
	return out
}
