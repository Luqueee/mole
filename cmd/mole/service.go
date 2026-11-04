// `mole service`: start mole at login under the platform's service manager
// and restart it if it dies — a launchd user agent on macOS, a systemd user
// unit on Linux. The service runs `mole up -config <absolute path>` in the
// foreground with MOLE_SERVICE=1 set; the manager supervises the process and
// its output goes to the same log `mole up -d` uses (see `mole logs`).
//
// The manager restarts mole only when it fails (a crash, a non-zero exit). A
// clean stop — `mole down`, SIGTERM — is respected until the next login, so
// `mole down` keeps working on a supervised mole.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Luqueee/mole/internal/config"
)

// serviceEnv marks a mole started by the service manager (see claimInstance).
const serviceEnv = "MOLE_SERVICE"

const (
	launchdLabel    = "dev.luqueee.mole"
	systemdUnitName = "mole.service"
)

// serviceSpec is what the installed service runs.
type serviceSpec struct {
	exe    string // absolute path of the mole binary
	config string // absolute path of the config file
	log    string // file that receives stdout and stderr
	// env holds KEY=VALUE pairs the service needs besides serviceEnv: the
	// XDG directories set where install ran, so the supervised mole uses
	// the same state directory (pidfile, lock, log) as the CLI.
	env []string
}

// serviceEnvVars are passed on to the service when set at install time.
var serviceEnvVars = []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME"}

// runner runs a service-manager command and returns its trimmed output.
type runner func(name string, args ...string) (string, error)

func execRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// dryRunner prints each command instead of running it.
func dryRunner(w io.Writer) runner {
	return func(name string, args ...string) (string, error) {
		fmt.Fprintln(w, "  "+shellJoin(append([]string{name}, args...)))
		return "", nil
	}
}

// serviceManager is one platform's service manager.
type serviceManager interface {
	describe() string // e.g. "launchd user agent dev.luqueee.mole"
	unitPath() string // the file install writes
	render(spec serviceSpec) string
	// install (re)loads the written file and starts the service.
	install(run runner) error
	// uninstall stops and unloads the service; remove deletes the file.
	uninstall(run runner, remove func() error) error
	// start starts the installed service.
	start(run runner) error
	// runningPid is the PID the manager runs mole as; 0 when not running.
	runningPid(query runner) int
	// notes is advice printed after a successful install.
	notes() []string
}

var errServiceUnsupported = fmt.Errorf("mole service is not supported on %s yet; start mole at login with 'mole up -d' instead (for example from a logon script or the desktop app)", runtime.GOOS)

// platformService returns this platform's service manager.
func platformService() (serviceManager, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return launchdAgent{home: home, uid: os.Getuid()}, nil
	case "linux":
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			base = filepath.Join(home, ".config")
		}
		return systemdUser{configHome: base, user: currentUserName()}, nil
	default:
		return nil, errServiceUnsupported
	}
}

// installedService returns the platform's service manager when the mole
// service is installed, else nil.
func installedService() serviceManager {
	svc, err := platformService()
	if err != nil {
		return nil
	}
	if _, err := os.Stat(svc.unitPath()); err != nil {
		return nil
	}
	return svc
}

func runService(args []string) int {
	usage := func() {
		fmt.Fprintln(os.Stderr, `Usage: mole service install|uninstall|status [flags]

Start mole at login and restart it if it dies, under the system's service
manager: a launchd user agent on macOS, a systemd user unit on Linux. The
service runs 'mole up -config PATH' in the foreground; its output goes to
the mole log ('mole logs'). 'mole down' stops it until the next login.

Flags:
  -config   config the service uses (default: ./mole.yaml, then user-global);
            resolved to an absolute path at install time
  -dry-run  print the service file and the commands instead of running them`)
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		usage()
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	fs.Usage = usage
	configPath := fs.String("config", "", "config file the service uses")
	dryRun := fs.Bool("dry-run", false, "print instead of running")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "mole service %s: unexpected argument %q\n", sub, fs.Arg(0))
		return 2
	}
	switch sub {
	case "install", "uninstall", "status":
	default:
		fmt.Fprintf(os.Stderr, "mole service: unknown subcommand %q\n\n", sub)
		usage()
		return 2
	}

	svc, err := platformService()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch sub {
	case "install":
		spec, err := resolveServiceSpec(*configPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if *dryRun {
			return serviceInstall(svc, spec, true, os.Stdout, dryRunner(os.Stdout), execRunner)
		}
		return serviceInstall(svc, spec, false, os.Stdout, execRunner, execRunner)
	case "uninstall":
		if *dryRun {
			return serviceUninstall(svc, true, os.Stdout, dryRunner(os.Stdout))
		}
		return serviceUninstall(svc, false, os.Stdout, execRunner)
	default:
		return serviceStatus(svc, os.Stdout, execRunner)
	}
}

// resolveServiceSpec fixes the binary and config the service runs. Both
// must be absolute: a service manager starts mole from another directory.
func resolveServiceSpec(configFlag string) (serviceSpec, error) {
	exe, err := serviceExecutable()
	if err != nil {
		return serviceSpec{}, fmt.Errorf("cannot locate the mole binary: %w", err)
	}
	path := configFlag
	if path == "" {
		path = config.Find()
	}
	if path == "" {
		return serviceSpec{}, errors.New("no config file found; run 'mole init' first or pass -config PATH")
	}
	if path, err = filepath.Abs(path); err != nil {
		return serviceSpec{}, err
	}
	if _, err := os.Stat(path); err != nil {
		return serviceSpec{}, fmt.Errorf("config %s: %w", path, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return serviceSpec{}, err
	}
	if cfg.Remote == "" {
		return serviceSpec{}, fmt.Errorf("no remote configured in %s; run 'mole init' or set 'remote:' first", path)
	}
	spec := serviceSpec{exe: exe, config: path, log: logPath()}
	for _, k := range serviceEnvVars {
		if v := os.Getenv(k); v != "" {
			spec.env = append(spec.env, k+"="+v)
		}
	}
	for _, p := range append([]string{spec.exe, spec.config, spec.log}, spec.env...) {
		if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return serviceSpec{}, fmt.Errorf("path %q contains control characters", p)
		}
	}
	return spec, nil
}

// serviceExecutable is the absolute path of this binary. A PATH entry that
// resolves to the same file (e.g. Homebrew's bin symlink) is preferred: it
// survives upgrades that move the real file.
func serviceExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return "", err
	}
	if p, err := exec.LookPath(filepath.Base(exe)); err == nil {
		if p, err = filepath.Abs(p); err == nil && sameFile(p, exe) {
			return p, nil
		}
	}
	return exe, nil
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

func serviceInstall(svc serviceManager, spec serviceSpec, dryRun bool, out io.Writer, run, query runner) int {
	content := svc.render(spec)
	// A mole the manager does not run would keep the service's mole
	// waiting for the instance lock: hand over to the service.
	foreign := 0
	if d := findDaemon(); d.running && d.pid != 0 && d.pid != svc.runningPid(query) {
		foreign = d.pid
	}

	if dryRun {
		fmt.Fprintf(out, "# %s\n%s\n", svc.unitPath(), content)
		fmt.Fprintln(out, "# commands:")
		if foreign != 0 {
			fmt.Fprintf(out, "  mole down   # stop the running mole (pid %d) so the service takes over\n", foreign)
		}
		_ = svc.install(run)
		return 0
	}

	for _, dir := range []string{filepath.Dir(svc.unitPath()), filepath.Dir(spec.log)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	if err := os.WriteFile(svc.unitPath(), []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error: write service file:", err)
		return 1
	}
	if foreign != 0 {
		fmt.Fprintf(out, "stopping the running mole (pid %d) so the service takes over\n", foreign)
		if err := stopDaemon(foreign); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not stop pid %d: %v; the service starts once it exits\n", foreign, err)
		} else {
			removePidFile(foreign)
		}
	}
	if err := svc.install(run); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintf(os.Stderr, "the service file was written to %s; 'mole service uninstall' removes it\n", svc.unitPath())
		return 1
	}
	fmt.Fprintf(out, "mole service installed: %s\n", svc.describe())
	fmt.Fprintf(out, "  file:   %s\n", svc.unitPath())
	fmt.Fprintf(out, "  runs:   %s\n", shellJoin([]string{spec.exe, "up", "-config", spec.config}))
	fmt.Fprintf(out, "  logs:   %s ('mole logs')\n", spec.log)
	fmt.Fprintln(out, "  status: mole service status")
	for _, n := range svc.notes() {
		fmt.Fprintln(out, "  tip:   ", n)
	}
	return 0
}

func serviceUninstall(svc serviceManager, dryRun bool, out io.Writer, run runner) int {
	if _, err := os.Stat(svc.unitPath()); err != nil {
		fmt.Fprintf(os.Stderr, "mole service is not installed (no %s)\n", svc.unitPath())
		return 1
	}
	remove := func() error { return os.Remove(svc.unitPath()) }
	if dryRun {
		fmt.Fprintln(out, "# commands:")
		remove = func() error {
			fmt.Fprintln(out, "  "+shellJoin([]string{"rm", svc.unitPath()}))
			return nil
		}
	}
	if err := svc.uninstall(run, remove); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if !dryRun {
		fmt.Fprintf(out, "mole service uninstalled (%s); mole no longer starts at login\n", svc.describe())
	}
	return 0
}

func serviceStatus(svc serviceManager, out io.Writer, query runner) int {
	if _, err := os.Stat(svc.unitPath()); err != nil {
		fmt.Fprintf(out, "mole service: not installed (%s)\n", svc.describe())
		fmt.Fprintln(out, "  install it with 'mole service install'")
		return 1
	}
	fmt.Fprintf(out, "mole service: installed (%s)\n", svc.describe())
	fmt.Fprintf(out, "  file:  %s\n", svc.unitPath())
	pid := svc.runningPid(query)
	if pid != 0 {
		fmt.Fprintf(out, "  state: running (pid %d)\n", pid)
	} else {
		fmt.Fprintln(out, "  state: not running (starts at next login; 'mole restart' or reinstall to start it now)")
	}
	if d := findDaemon(); d.running && d.pid != 0 && d.pid != pid {
		fmt.Fprintf(out, "  note:  a mole outside the service is running (pid %d)\n", d.pid)
	}
	return 0
}

// printServiceNoteAfterDown tells the user how the login service relates
// to the mole `mole down` just stopped.
func printServiceNoteAfterDown(stopped int) {
	svc := installedService()
	if svc == nil {
		return
	}
	if pid := svc.runningPid(execRunner); pid != 0 && pid != stopped {
		fmt.Fprintf(os.Stderr, "note: the mole login service (pid %d) is still running and takes over; 'mole service uninstall' stops and removes it\n", pid)
		return
	}
	fmt.Fprintln(os.Stderr, "note: mole is installed as a login service and starts again at the next login; 'mole service uninstall' removes it")
}

// restartManagedService restarts pid through the service manager when the
// login service runs it, so the new mole stays supervised. handled is
// false when pid is not the service's.
func restartManagedService(pid int) (handled bool, code int) {
	svc := installedService()
	if svc == nil || svc.runningPid(execRunner) != pid {
		return false, 0
	}
	if err := stopDaemon(pid); err != nil {
		fmt.Fprintf(os.Stderr, "could not stop pid %d: %v\n", pid, err)
		return true, 1
	}
	removePidFile(pid)
	if err := svc.start(execRunner); err != nil {
		fmt.Fprintf(os.Stderr, "error: mole was stopped, but starting the login service failed: %v\n", err)
		return true, 1
	}
	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if d := findDaemon(); d.running && d.pid != pid {
			fmt.Printf("mole restarted by the login service (pid %d)\n", d.pid)
			return true, 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "the login service was started, but mole is not up yet; see 'mole service status' and 'mole logs'")
	return true, 1
}

// bootstrapRetryDelay spaces launchctl bootstrap attempts.
var bootstrapRetryDelay = time.Second

// launchdAgent is a per-user launchd agent in ~/Library/LaunchAgents.
type launchdAgent struct {
	home string
	uid  int
}

func (l launchdAgent) describe() string { return "launchd user agent " + launchdLabel }

func (l launchdAgent) unitPath() string {
	return filepath.Join(l.home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func (l launchdAgent) domain() string { return "gui/" + strconv.Itoa(l.uid) }
func (l launchdAgent) target() string { return l.domain() + "/" + launchdLabel }

// render returns the agent's plist. KeepAlive restarts mole when it exits
// unsuccessfully (a crash, a startup error) but not after a clean stop.
func (l launchdAgent) render(spec serviceSpec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Installed by 'mole service install'; remove with 'mole service uninstall'. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range []string{spec.exe, "up", "-config", spec.config} {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>` + serviceEnv + `</key>
		<string>1</string>
`)
	for _, kv := range spec.env {
		k, v, _ := strings.Cut(kv, "=")
		b.WriteString("\t\t<key>" + xmlEscape(k) + "</key>\n\t\t<string>" + xmlEscape(v) + "</string>\n")
	}
	b.WriteString(`	</dict>
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
	<string>` + xmlEscape(spec.log) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(spec.log) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func (l launchdAgent) install(run runner) error {
	// Unload a previous version first; this fails harmlessly when none is
	// loaded.
	_, _ = run("launchctl", "bootout", l.target())
	var out string
	var err error
	// bootstrap can fail briefly while a booted-out instance shuts down.
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(bootstrapRetryDelay)
		}
		if out, err = run("launchctl", "bootstrap", l.domain(), l.unitPath()); err == nil {
			return nil
		}
	}
	// Older launchctl, or a domain bootstrap refuses: the legacy verb.
	if _, lerr := run("launchctl", "load", "-w", l.unitPath()); lerr == nil {
		return nil
	}
	return fmt.Errorf("launchctl bootstrap %s: %v: %s", l.domain(), err, out)
}

func (l launchdAgent) uninstall(run runner, remove func() error) error {
	// bootout stops mole (SIGTERM) and unloads the agent; neither verb
	// matters when the agent is not loaded.
	if _, err := run("launchctl", "bootout", l.target()); err != nil {
		_, _ = run("launchctl", "unload", "-w", l.unitPath())
	}
	return remove()
}

func (l launchdAgent) start(run runner) error {
	if out, err := run("launchctl", "kickstart", l.target()); err != nil {
		return fmt.Errorf("launchctl kickstart: %v: %s", err, out)
	}
	return nil
}

func (l launchdAgent) runningPid(query runner) int {
	out, err := query("launchctl", "print", l.target())
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "pid = "); ok {
			if pid, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && pid > 0 {
				return pid
			}
		}
	}
	return 0
}

func (l launchdAgent) notes() []string { return nil }

// systemdUser is a systemd user unit.
type systemdUser struct {
	configHome string // $XDG_CONFIG_HOME or ~/.config
	user       string // login name, for the linger hint
}

func (s systemdUser) describe() string { return "systemd user unit " + systemdUnitName }

func (s systemdUser) unitPath() string {
	return filepath.Join(s.configHome, "systemd", "user", systemdUnitName)
}

// render returns the unit. Restart=on-failure restarts mole when it
// fails but not after a clean stop. network-online.target only orders
// against the user manager's own targets; the startup retry in `mole up`
// is what copes with a network that is not up yet.
func (s systemdUser) render(spec serviceSpec) string {
	env := ""
	for _, kv := range spec.env {
		env += "Environment=" + systemdQuoteEnv(kv) + "\n"
	}
	return `# Installed by 'mole service install'; remove with 'mole service uninstall'.
[Unit]
Description=mole: forward local ports over SSH
Documentation=https://github.com/Luqueee/mole
After=network-online.target

[Service]
Type=simple
Environment=` + serviceEnv + `=1
` + env + `ExecStart=` + systemdQuote(spec.exe) + ` up -config ` + systemdQuote(spec.config) + `
Restart=on-failure
RestartSec=10
StandardOutput=append:` + systemdEscapePath(spec.log) + `
StandardError=append:` + systemdEscapePath(spec.log) + `

[Install]
WantedBy=default.target
`
}

func (s systemdUser) systemctl(run runner, args ...string) error {
	if out, err := run("systemctl", append([]string{"--user"}, args...)...); err != nil {
		return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

func (s systemdUser) install(run runner) error {
	if err := s.systemctl(run, "daemon-reload"); err != nil {
		return err
	}
	if err := s.systemctl(run, "enable", systemdUnitName); err != nil {
		return err
	}
	// restart rather than start, so a reinstall runs the new unit.
	return s.systemctl(run, "restart", systemdUnitName)
}

func (s systemdUser) uninstall(run runner, remove func() error) error {
	// Stops mole and removes the login hook; irrelevant if already gone.
	_ = s.systemctl(run, "disable", "--now", systemdUnitName)
	if err := remove(); err != nil {
		return err
	}
	return s.systemctl(run, "daemon-reload")
}

func (s systemdUser) start(run runner) error { return s.systemctl(run, "start", systemdUnitName) }

func (s systemdUser) runningPid(query runner) int {
	out, err := query("systemctl", "--user", "show", "-p", "MainPID", "--value", systemdUnitName)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func (s systemdUser) notes() []string {
	var notes []string
	if s.user != "" {
		if _, err := os.Stat(filepath.Join("/var/lib/systemd/linger", s.user)); err != nil {
			notes = append(notes, fmt.Sprintf("user services run only while you are logged in; on a headless machine run 'loginctl enable-linger %s' so mole starts at boot", s.user))
		}
	}
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		notes = append(notes, "the service uses ssh-agent only if the systemd user manager knows SSH_AUTH_SOCK (see 'systemctl --user show-environment'); otherwise it uses ~/.ssh/id_* keys")
	}
	return notes
}

func currentUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// xmlEscape escapes text for an XML element.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// systemdQuote quotes one ExecStart argument: backslashes and double quotes
// are escaped inside the quotes, and % (specifiers) and $ (variable
// expansion) are doubled so they stay literal.
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

// systemdQuoteEnv quotes one Environment= assignment, which expands
// specifiers but not variables.
func systemdQuoteEnv(kv string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(kv) + `"`
}

// systemdEscapePath escapes a path in a value that expands specifiers but
// is not split into words (StandardOutput=append:PATH).
func systemdEscapePath(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// shellJoin renders a command for display, quoting arguments a POSIX
// shell would split or expand.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}
