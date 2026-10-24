package main

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// osNotify shows a desktop notification, best effort: the daemon runs in the
// background, where a transfer waiting for an answer would otherwise go
// unnoticed. The text reaches the notifier as an argument, never spliced into
// a script, because it contains file names chosen by the sender.
func osNotify(title, text string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("osascript",
			"-e", "on run argv",
			"-e", "display notification (item 1 of argv) with title (item 2 of argv)",
			"-e", "end run",
			text, title)
	case "linux":
		path, err := exec.LookPath("notify-send")
		if err != nil {
			return
		}
		cmd = exec.Command(path, "--", title, text)
	default:
		return
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}

// offerSummary names what is being offered, briefly: "a.txt, b.txt and 2 more (3.1 MiB)".
func offerSummary(names []string, total int64) string {
	const show = 2
	var b strings.Builder
	for i, n := range names {
		if i == show {
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(n)
	}
	if len(names) > show {
		b.WriteString(" and ")
		b.WriteString(strconv.Itoa(len(names) - show))
		b.WriteString(" more")
	}
	b.WriteString(" (" + humanSize(total) + ")")
	out := b.String()
	if r := []rune(out); len(r) > 120 {
		out = string(r[:117]) + "..."
	}
	return out
}
