package brew

import "testing"

func TestFormula(t *testing.T) {
	for path, want := range map[string]string{
		"/opt/homebrew/Cellar/mole-tunnel/0.7.0/bin/mole":              "mole-tunnel",
		"/usr/local/Cellar/mole-tunnel/0.7.0/bin/mole":                 "mole-tunnel",
		"/home/linuxbrew/.linuxbrew/Cellar/mole-tunnel/0.7.0/bin/mole": "mole-tunnel",
		"/opt/homebrew/Cellar/mole/1.44.1/bin/mole":                    "mole",
		"/Users/adria/.local/bin/mole":                                 "",
		"/opt/homebrew/bin/mole":                                       "", // the link, not the file: callers resolve it first
		"/Users/adria/Cellar/notes/mole":                               "", // a Cellar directory without a version below it
		"/Users/adria/Documents/Cellar/mole":                           "",
		"/Applications/mole.app/Contents/MacOS/mole":                   "",
		"": "",
	} {
		got, ok := Formula(path)
		if got != want || ok != (want != "") {
			t.Errorf("Formula(%q) = %q, %v; want %q", path, got, ok, want)
		}
	}
}
