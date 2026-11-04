package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testSpec = serviceSpec{
	exe:    "/usr/local/bin/mole",
	config: "/Users/me/.config/mole/config.yaml",
	log:    "/Users/me/.local/state/mole/mole.log",
}

func TestLaunchdPlist_Golden(t *testing.T) {
	got := launchdAgent{home: "/Users/me", uid: 501}.render(testSpec)
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Installed by 'mole service install'; remove with 'mole service uninstall'. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.luqueee.mole</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/mole</string>
		<string>up</string>
		<string>-config</string>
		<string>/Users/me/.config/mole/config.yaml</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>MOLE_SERVICE</key>
		<string>1</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>/Users/me/.local/state/mole/mole.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/me/.local/state/mole/mole.log</string>
</dict>
</plist>
`
	if got != want {
		t.Errorf("plist mismatch\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestSystemdUnit_Golden(t *testing.T) {
	got := systemdUser{configHome: "/home/me/.config"}.render(serviceSpec{
		exe:    "/usr/local/bin/mole",
		config: "/home/me/.config/mole/config.yaml",
		log:    "/home/me/.local/state/mole/mole.log",
	})
	want := `# Installed by 'mole service install'; remove with 'mole service uninstall'.
[Unit]
Description=mole: forward local ports over SSH
Documentation=https://github.com/Luqueee/mole
After=network-online.target

[Service]
Type=simple
Environment=MOLE_SERVICE=1
ExecStart="/usr/local/bin/mole" up -config "/home/me/.config/mole/config.yaml"
Restart=on-failure
RestartSec=10
StandardOutput=append:/home/me/.local/state/mole/mole.log
StandardError=append:/home/me/.local/state/mole/mole.log

[Install]
WantedBy=default.target
`
	if got != want {
		t.Errorf("unit mismatch\n--- got\n%s\n--- want\n%s", got, want)
	}
}

// Paths with spaces and characters the service file formats treat
// specially must reach mole verbatim.
func TestServiceFiles_QuotePaths(t *testing.T) {
	spec := serviceSpec{
		exe:    "/opt/my tools/mole",
		config: `/home/a b/100%/"q"/$HOME\x & <y>.yaml`,
		log:    "/home/a b/state 100%/mole.log",
	}
	unit := systemdUser{}.render(spec)
	if want := `ExecStart="/opt/my tools/mole" up -config "/home/a b/100%%/\"q\"/$$HOME\\x & <y>.yaml"`; !strings.Contains(unit, want+"\n") {
		t.Errorf("unit ExecStart not quoted as %s:\n%s", want, unit)
	}
	if want := "StandardOutput=append:/home/a b/state 100%%/mole.log\n"; !strings.Contains(unit, want) {
		t.Errorf("unit StandardOutput not escaped as %q:\n%s", want, unit)
	}

	plist := launchdAgent{}.render(spec)
	for _, want := range []string{
		"<string>/opt/my tools/mole</string>",
		"<string>/home/a b/100%/&quot;q&quot;/$HOME\\x &amp; &lt;y&gt;.yaml</string>",
		"<string>/home/a b/state 100%/mole.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %s:\n%s", want, plist)
		}
	}
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/mole":        `"/usr/bin/mole"`,
		"/a b/c":               `"/a b/c"`,
		`/a"b`:                 `"/a\"b"`,
		`C:\x`:                 `"C:\\x"`,
		"/50%/$USER/${HOME}/x": `"/50%%/$$USER/$${HOME}/x"`,
	} {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"launchctl", "bootstrap", "gui/501", "/Users/a b/it's.plist", ""})
	want := `launchctl bootstrap gui/501 '/Users/a b/it'\''s.plist' ''`
	if got != want {
		t.Errorf("shellJoin = %s, want %s", got, want)
	}
}

// noQuery is a service-manager query that finds nothing running.
func noQuery(string, ...string) (string, error) { return "", errors.New("not loaded") }

func TestServiceInstall_DryRun(t *testing.T) {
	dir := isolateState(t)
	cases := []struct {
		svc  serviceManager
		cmds []string
	}{
		{launchdAgent{home: dir, uid: 501}, []string{
			"launchctl bootout gui/501/dev.luqueee.mole",
			"launchctl bootstrap gui/501 " + shellQuote(filepath.Join(dir, "Library", "LaunchAgents", "dev.luqueee.mole.plist")),
		}},
		{systemdUser{configHome: filepath.Join(dir, "config")}, []string{
			"systemctl --user daemon-reload",
			"systemctl --user enable mole.service",
			"systemctl --user restart mole.service",
		}},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if code := serviceInstall(c.svc, testSpec, true, &out, dryRunner(&out), noQuery); code != 0 {
			t.Fatalf("%s: dry-run install = %d", c.svc.describe(), code)
		}
		got := out.String()
		if !strings.HasPrefix(got, "# "+c.svc.unitPath()+"\n"+c.svc.render(testSpec)) {
			t.Errorf("%s: dry run does not print the service file first:\n%s", c.svc.describe(), got)
		}
		if want := "# commands:\n  " + strings.Join(c.cmds, "\n  ") + "\n"; !strings.HasSuffix(got, want) {
			t.Errorf("%s: dry-run commands:\n%s\nwant suffix:\n%s", c.svc.describe(), got, want)
		}
		if _, err := os.Stat(c.svc.unitPath()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: dry run wrote %s", c.svc.describe(), c.svc.unitPath())
		}
	}
}

func TestServiceUninstall_DryRun(t *testing.T) {
	dir := isolateState(t)
	svc := systemdUser{configHome: dir}
	var out bytes.Buffer
	if code := serviceUninstall(svc, true, &out, dryRunner(&out)); code != 1 {
		t.Errorf("uninstall of a missing service = %d, want 1", code)
	}

	if err := os.MkdirAll(filepath.Dir(svc.unitPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.unitPath(), []byte(svc.render(testSpec)), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := serviceUninstall(svc, true, &out, dryRunner(&out)); code != 0 {
		t.Fatalf("dry-run uninstall = %d", code)
	}
	want := "# commands:\n" +
		"  systemctl --user disable --now mole.service\n" +
		"  " + shellJoin([]string{"rm", svc.unitPath()}) + "\n" +
		"  systemctl --user daemon-reload\n"
	if out.String() != want {
		t.Errorf("dry-run uninstall output:\n%s\nwant:\n%s", out.String(), want)
	}
	if _, err := os.Stat(svc.unitPath()); err != nil {
		t.Errorf("dry run removed the unit: %v", err)
	}
}

func TestLaunchdInstall_FallsBackToLoad(t *testing.T) {
	saved := bootstrapRetryDelay
	bootstrapRetryDelay = 0
	t.Cleanup(func() { bootstrapRetryDelay = saved })
	var calls []string
	run := func(name string, args ...string) (string, error) {
		calls = append(calls, shellJoin(append([]string{name}, args...)))
		if len(args) > 0 && (args[0] == "bootstrap" || args[0] == "bootout") {
			return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
		}
		return "", nil
	}
	l := launchdAgent{home: "/Users/me", uid: 501}
	if err := l.install(run); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The plist path is built with filepath, so derive it the same way: on
	// Windows (where this test also runs) the separators are backslashes.
	if last := calls[len(calls)-1]; last != shellJoin([]string{"launchctl", "load", "-w", l.unitPath()}) {
		t.Errorf("last command = %s, want the legacy load fallback (calls: %q)", last, calls)
	}
}

func TestServiceRunningPid(t *testing.T) {
	launchdOut := "gui/501/dev.luqueee.mole = {\n\tactive count = 1\n\tpath = /Users/me/Library/LaunchAgents/dev.luqueee.mole.plist\n\tstate = running\n\tpid = 4242\n\tlast exit code = (never exited)\n}"
	l := launchdAgent{uid: 501}
	if pid := l.runningPid(func(string, ...string) (string, error) { return launchdOut, nil }); pid != 4242 {
		t.Errorf("launchd pid = %d, want 4242", pid)
	}
	if pid := l.runningPid(noQuery); pid != 0 {
		t.Errorf("launchd pid of an unloaded agent = %d, want 0", pid)
	}
	s := systemdUser{}
	for out, want := range map[string]int{"4242\n": 4242, "0": 0, "": 0} {
		if pid := s.runningPid(func(string, ...string) (string, error) { return out, nil }); pid != want {
			t.Errorf("systemd MainPID %q = %d, want %d", out, pid, want)
		}
	}
}

func TestRunService_Usage(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"bogus"}, {"install", "extra"}} {
		var code int
		_ = captureBoth(t, func() { code = runService(args) })
		if code != 2 {
			t.Errorf("runService(%q) = %d, want 2", args, code)
		}
	}
}

func TestResolveServiceSpec(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	if _, err := resolveServiceSpec(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing config was accepted")
	}
	noRemote := filepath.Join(dir, "noremote.yaml")
	if err := os.WriteFile(noRemote, []byte("auto_discover: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveServiceSpec(noRemote); err == nil || !strings.Contains(err.Error(), "no remote") {
		t.Errorf("config without a remote: err = %v", err)
	}
	good := filepath.Join(dir, "mole.yaml")
	if err := os.WriteFile(good, []byte("remote: dev@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	spec, err := resolveServiceSpec("mole.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(spec.exe) || !filepath.IsAbs(spec.config) || !sameFile(spec.config, good) {
		t.Errorf("spec paths not absolute or wrong: %+v", spec)
	}
	if spec.log != logPath() {
		t.Errorf("spec log = %s, want %s", spec.log, logPath())
	}
}

// The restart through the service manager must not wait on a service
// that does not run the mole being restarted.
func TestRestartManagedService_NotInstalled(t *testing.T) {
	isolateState(t)
	start := time.Now()
	if handled, _ := restartManagedService(12345); handled {
		t.Error("restart was routed to a service that is not installed")
	}
	if time.Since(start) > time.Second {
		t.Error("checking for the service took too long")
	}
}

// XDG directories set at install time reach the service, so it shares the
// CLI's state directory (pidfile, lock, log).
func TestServiceFiles_PassXDGEnvironment(t *testing.T) {
	spec := testSpec
	spec.env = []string{"XDG_STATE_HOME=/data/state 100%", `XDG_CONFIG_HOME=/cfg/"x"`}
	unit := systemdUser{}.render(spec)
	for _, want := range []string{
		"Environment=MOLE_SERVICE=1\n",
		`Environment="XDG_STATE_HOME=/data/state 100%%"` + "\n",
		`Environment="XDG_CONFIG_HOME=/cfg/\"x\""` + "\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	plist := launchdAgent{}.render(spec)
	for _, want := range []string{
		"\t\t<key>XDG_STATE_HOME</key>\n\t\t<string>/data/state 100%</string>\n",
		"\t\t<key>XDG_CONFIG_HOME</key>\n\t\t<string>/cfg/&quot;x&quot;</string>\n",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}

	t.Setenv("XDG_STATE_HOME", "/data/state")
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg := filepath.Join(t.TempDir(), "mole.yaml")
	if err := os.WriteFile(cfg, []byte("remote: dev@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveServiceSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.env) != 1 || got.env[0] != "XDG_STATE_HOME=/data/state" {
		t.Errorf("spec env = %q, want only XDG_STATE_HOME", got.env)
	}
}
