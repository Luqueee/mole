package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

func TestParseLogfmt(t *testing.T) {
	line := `time=2026-06-29T20:08:54.510+02:00 level=INFO msg="no dev ports up yet" recheck=15s remote=dev`
	got := parseLogfmt(line)
	want := [][2]string{
		{"time", "2026-06-29T20:08:54.510+02:00"},
		{"level", "INFO"},
		{"msg", "no dev ports up yet"},
		{"recheck", "15s"},
		{"remote", "dev"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d pairs, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pair %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFormatLine_Plain(t *testing.T) {
	line := `time=2026-06-29T20:08:54.510+02:00 level=WARN msg="could not bind" port=20241`
	out := formatLine(line, false)
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain output must not contain ANSI escapes: %q", out)
	}
	for _, want := range []string{"20:08:54", "[WARN]", "could not bind", "port=20241"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain output missing %q: %q", want, out)
		}
	}
}

func TestFormatLine_Color(t *testing.T) {
	line := `time=2026-06-29T20:08:54.510+02:00 level=ERROR msg="boom" err=x`
	out := formatLine(line, true)
	if !strings.Contains(out, "\x1b[") {
		t.Error("colour output should contain ANSI escapes")
	}
	// Red badge background for ERROR.
	if !strings.Contains(out, "48;2;248;81;73") {
		t.Errorf("ERROR badge colour missing: %q", out)
	}
}

func TestFormatLine_NonLogfmt(t *testing.T) {
	line := "panic: runtime error: something"
	if out := formatLine(line, true); out != line {
		t.Errorf("non-logfmt line should pass through unchanged: %q", out)
	}
}

func TestCollapseKey_IgnoresTime(t *testing.T) {
	a := `time=2026-06-29T20:00:00Z level=WARN msg="x" port=25`
	b := `time=2026-06-29T20:05:00Z level=WARN msg="x" port=25`
	if collapseKey(a) != collapseKey(b) {
		t.Error("lines differing only in timestamp should share a collapse key")
	}
	c := `time=2026-06-29T20:00:00Z level=WARN msg="x" port=26`
	if collapseKey(a) == collapseKey(c) {
		t.Error("lines differing in a non-time field must not collapse")
	}
}

func TestCollapser_CountsRepeats(t *testing.T) {
	var got []string
	out := captureStdout(t, func() {
		col := &collapser{color: false, dedup: true}
		col.emit(`time=2026-06-29T20:00:00Z level=WARN msg="dup" port=25`)
		col.emit(`time=2026-06-29T20:00:15Z level=WARN msg="dup" port=25`)
		col.emit(`time=2026-06-29T20:00:30Z level=WARN msg="dup" port=25`)
		col.emit(`time=2026-06-29T20:00:45Z level=INFO msg="other"`)
		col.flush()
	})
	got = nil
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		got = append(got, l)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 output lines, got %d: %q", len(got), got)
	}
	if !strings.Contains(got[0], "(×3)") {
		t.Errorf("first line should carry (×3): %q", got[0])
	}
	if strings.Contains(got[1], "×") {
		t.Errorf("non-repeated line must not carry a count: %q", got[1])
	}
}
