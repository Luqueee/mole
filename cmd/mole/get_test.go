package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSplitTarget(t *testing.T) {
	for _, tc := range []struct {
		arg, node, path string
		bad             bool
	}{
		{"devlabs:/var/log/syslog", "devlabs", "/var/log/syslog", false},
		{"devlabs:notes.txt", "devlabs", "notes.txt", false},
		{"dev@host:~/a:b.txt", "dev@host", "~/a:b.txt", false},
		{"nopath", "", "", true},
		{":/etc/hosts", "", "", true},
		{"devlabs:", "", "", true},
		{"-n:/etc/hosts", "", "", true},
	} {
		node, p, err := splitTarget(tc.arg)
		if tc.bad {
			if err == nil {
				t.Errorf("splitTarget(%q) accepted", tc.arg)
			}
			continue
		}
		if err != nil || node != tc.node || p != tc.path {
			t.Errorf("splitTarget(%q) = %q, %q, %v; want %q, %q", tc.arg, node, p, err, tc.node, tc.path)
		}
	}
}

// remoteSh runs the command line mole would send to the other machine, with
// the local shell standing in for the SSH server's.
func remoteSh(t *testing.T, home, script string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", shellCommand(script, args...))
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Dir = home // an SSH command starts in the user's home folder
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), strings.TrimSpace(errb.String()), code
}

func TestGetScriptsAgainstARealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripts run on the other machine's POSIX shell")
	}
	for _, tool := range []string{"sh", "tail", "cat", "wc", "stat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
	}
	home := t.TempDir()
	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i*7 + i/251)
	}
	// A name with spaces, quotes and a leading dash must stay data.
	name := `-my 'big' "file".bin`
	file := filepath.Join(home, "dir with space", name)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o640); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(file, mt, mt); err != nil {
		t.Fatal(err)
	}
	rel := "~/dir with space/" + name

	for _, p := range []string{file, rel} {
		out, errOut, code := remoteSh(t, home, getStatScript, p)
		if code != 0 {
			t.Fatalf("stat %q: exit %d: %s", p, code, errOut)
		}
		f := strings.Fields(out)
		if len(f) != 3 || f[0] != strconv.Itoa(len(data)) || f[1] != "1700000000" || f[2] != "640" {
			t.Fatalf("stat %q = %q, want size, mtime and mode of the file", p, out)
		}
		for _, off := range []int{0, 1, 1234, len(data)} {
			got, errOut, code := remoteSh(t, home, getOpenScript, p, strconv.Itoa(off))
			if code != 0 || got != string(data[off:]) {
				t.Fatalf("open %q from %d: exit %d (%s), %d bytes, want %d", p, off, code, errOut, len(got), len(data)-off)
			}
		}
		sum, _, code := remoteSh(t, home, getSumScript, p)
		want := sha256.Sum256(data)
		if code == getNoSumStatus {
			t.Log("no sha256 tool on this machine: the missing-checksum path is what mole reports")
			continue
		}
		if code != 0 || !strings.HasPrefix(sum, hex.EncodeToString(want[:])) {
			t.Fatalf("sum %q: exit %d, %q", p, code, sum)
		}
	}
}

func TestGetStatScriptRefusesWhatIsNotAReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripts run on the other machine's POSIX shell")
	}
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path     string
		code     int
		contains string
	}{
		{"missing.txt", 3, "no such file"},
		{"folder", 4, "directory"},
		{"~/folder", 4, "directory"},
	} {
		_, errOut, code := remoteSh(t, home, getStatScript, tc.path)
		if code != tc.code || !strings.Contains(errOut, tc.contains) {
			t.Errorf("stat %q: exit %d %q, want exit %d mentioning %q", tc.path, code, errOut, tc.code, tc.contains)
		}
	}
}
