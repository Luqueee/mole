package main

import (
	"io"
	"strings"
	"testing"

	"github.com/Luqueee/mole/internal/config"
)

func TestClipListenForURL(t *testing.T) {
	cases := map[string]string{
		"http://100.64.0.10:7777": "100.64.0.10:7777",
		"100.64.0.10:8888":        "100.64.0.10:8888",
		"https://[fd00::10]:9000": "[fd00::10]:9000",
		"http://100.64.0.10":      "100.64.0.10:7777",
		"":                        config.DefaultClipListen,
		"not a URL":               config.DefaultClipListen,
	}
	for raw, want := range cases {
		if got := clipListenForURL(raw); got != want {
			t.Errorf("clipListenForURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestGatherAnswers_ClipListenEnvironmentOverridesDefault(t *testing.T) {
	t.Setenv("MOLE_CLIP_LISTEN", "100.64.0.20:7777")

	ans, err := gatherAnswers(initInputs{
		Remote:         "dev",
		PortsCSV:       "3000",
		ClipEnabled:    true,
		ClipURL:        "http://100.64.0.10:7777",
		ClipIntervalMs: 500,
	}, initOptions{})
	if err != nil {
		t.Fatalf("gatherAnswers() error = %v", err)
	}
	if ans.ClipListen != "100.64.0.20:7777" {
		t.Fatalf("ClipListen = %q, want environment value", ans.ClipListen)
	}

	t.Setenv("MOLE_CLIP_LISTEN", "")
	ans, err = gatherAnswers(initInputs{
		Remote:         "dev",
		PortsCSV:       "3000",
		ClipEnabled:    true,
		ClipURL:        "http://100.64.0.10:8888",
		ClipIntervalMs: 500,
	}, initOptions{})
	if err != nil {
		t.Fatalf("gatherAnswers() error = %v", err)
	}
	if ans.ClipListen != "100.64.0.10:8888" {
		t.Fatalf("ClipListen = %q, want address derived from URL", ans.ClipListen)
	}
}

func TestGatherAnswers_InteractiveClipAfterEveryPortChoice(t *testing.T) {
	t.Setenv("MOLE_CLIP", "")
	t.Setenv("MOLE_CLIP_URL", "")
	t.Setenv("MOLE_CLIP_LISTEN", "")

	cases := map[string]string{
		"auto-discover": "dev\n1\ny\n\n\n\n",
		"explicit":      "dev\n2\n3000\ny\n\n\n\n",
		"skip":          "dev\n3\ny\n\n\n\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			ans, err := gatherAnswers(initInputs{}, initOptions{
				Interactive: true,
				In:          &oneLineReader{lines: strings.Split(input, "\n")},
				Out:         &strings.Builder{},
			})
			if err != nil {
				t.Fatalf("gatherAnswers() error = %v", err)
			}
			if !ans.ClipEnabled {
				t.Fatal("ClipEnabled = false, want true")
			}
			if ans.ClipURL != "http://100.64.0.10:7777" {
				t.Fatalf("ClipURL = %q, want default private endpoint", ans.ClipURL)
			}
			if ans.ClipListen != "100.64.0.10:7777" {
				t.Fatalf("ClipListen = %q, want address derived from ClipURL", ans.ClipListen)
			}
		})
	}
}

// oneLineReader prevents readLine's short-lived scanners from buffering the
// answers for later prompts in an interactive test.
type oneLineReader struct {
	lines []string
}

func (r *oneLineReader) Read(p []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, io.EOF
	}
	line := r.lines[0] + "\n"
	r.lines = r.lines[1:]
	copy(p, line)
	return len(line), nil
}
