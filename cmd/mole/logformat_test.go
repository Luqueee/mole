package main

import (
	"strings"
	"testing"
)

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
