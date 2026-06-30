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

// badgeWidth is the inner label width every badge is padded to, so the
// message column stays aligned across levels and event badges.
const badgeWidth = 7

type badgeStyle struct {
	label       string
	fr, fgc, fb int // foreground rgb
	br, bg, bb  int // background rgb
}

// styleFor picks a badge by event first, then by level. Specific events
// (like a port being forwarded) get their own colour so they stand out
// from the generic INFO stream.
func styleFor(level, msg string) badgeStyle {
	switch strings.ToLower(msg) {
	case "forwarding":
		return badgeStyle{"FORWARD", 16, 24, 16, 63, 185, 80} // dark on green
	case "unforwarding":
		return badgeStyle{"UNFWD", 24, 16, 16, 191, 97, 63} // dark on burnt orange
	}
	switch strings.ToUpper(level) {
	case "ERROR":
		return badgeStyle{"ERROR", 255, 255, 255, 248, 81, 73} // white on red
	case "WARN":
		return badgeStyle{"WARN", 20, 20, 20, 210, 153, 34} // black on amber
	case "INFO":
		return badgeStyle{"INFO", 255, 255, 255, 56, 139, 253} // white on blue
	case "DEBUG":
		return badgeStyle{"DEBUG", 230, 237, 243, 87, 96, 106} // light on gray
	default:
		return badgeStyle{strings.ToUpper(level), 230, 237, 243, 87, 96, 106}
	}
}

func renderBadge(st badgeStyle, color bool) string {
	if !color {
		return "[" + st.label + "]"
	}
	label := fmt.Sprintf(" %-*s ", badgeWidth, st.label)
	return badge(st.fr, st.fgc, st.fb, st.br, st.bg, st.bb, label)
}

// collapser folds consecutive identical log lines (ignoring their
// timestamp) into a single rendered line with a "(×N)" repeat counter.
//
// In buffered mode (piping, static `mole logs`) it holds the current run
// and prints it on flush — when a differing line arrives or at end of
// input. In live mode (`mole logs -f` on a TTY) it prints the first line
// of a run immediately and rewrites it in place with carriage return as
// repeats arrive, so warns seconds apart collapse onto one updating line
// instead of being split by the follow loop's idle polling.
type collapser struct {
	color bool
	raw   bool
	dedup bool
	live  bool // follow on a TTY: rewrite the run line in place with \r

	key   string // identity of the current run (line minus timestamp)
	first string // rendered text of the run's first line
	count int
	open  bool // live: a run line is on screen, not yet newline-terminated
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
		if c.live {
			c.rewrite()
		}
		return
	}
	c.seal()
	c.key = k
	c.first = c.render(line)
	c.count = 1
	if c.live {
		fmt.Print(c.first)
		c.open = true
	}
}

// seal terminates the current run. Buffered: print the run's line with
// its repeat count. Live: the line is already on screen, so just end it
// with a newline.
func (c *collapser) seal() {
	if c.count == 0 {
		return
	}
	if c.live {
		if c.open {
			fmt.Print("\n")
			c.open = false
		}
		c.count = 0
		return
	}
	out := c.first
	if c.count > 1 {
		out += countSuffix(c.count, c.color)
	}
	fmt.Println(out)
	c.count = 0
}

// rewrite redraws the current live run in place: carriage return, the
// first line + (×N) counter, then clear-to-end-of-line so a shorter
// redraw doesn't leave stale characters behind.
func (c *collapser) rewrite() {
	out := c.first
	if c.count > 1 {
		out += countSuffix(c.count, c.color)
	}
	fmt.Print("\r" + out + "\x1b[K")
}

// flush terminates and emits the current run. Used at end of input and
// by static (non-follow) rendering.
func (c *collapser) flush() {
	c.seal()
}

// idle is called when following and the log has momentarily run dry. In
// live (TTY) mode it does nothing, so a run stays open and repeats
// arriving seconds apart keep updating the same line in place instead of
// being split into separate (×N) lines. In non-live mode it flushes,
// preserving the previous piped-follow behaviour.
func (c *collapser) idle() {
	if c.live {
		return
	}
	c.flush()
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

	// Level / event badge.
	if level != "" {
		b.WriteString(renderBadge(styleFor(level, msg), color))
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
