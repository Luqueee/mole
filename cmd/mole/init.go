// mole init — interactive (or scripted) configuration.
//
// One source of truth for the configuration questions, shared by
// every installer and shell. Run after `mole` is installed, or
// re-run any time to regenerate the config.
//
// Usage:
//
//	mole init                          # interactive
//	mole init -remote dev@workstation  # semi-interactive
//	mole init -no-prompt -remote ...   # fully scripted (CI)
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Luqueee/mole/internal/config"
)

// initAnswers holds the user's choices, populated either by flags /
// env vars (non-interactive) or by the prompts in promptAnswers.
type initAnswers struct {
	Remote       string // "user@host[:port]"
	Ports        []int  // explicit ports, may be empty
	AutoDiscover bool   // probe remote for common dev ports
	ConfigPath   string // where to write the YAML
	PrintOnly    bool   // true → write to stdout instead of a file
	Global       bool   // true → ~/.config/mole/config.yaml
	ClipEnabled  bool   // true → also write clip_* keys for `mole clip`
	ClipURL      string // URL the LXC pulls from; e.g. http://100.64.0.10:7777
	ClipListen   string // address the Mac serves on; defaults to 127.0.0.1:7777
	ClipIntervalMs int  // clipboard poll cadence on the Mac
}

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	var (
		remote    = fs.String("remote", "", "SSH target, e.g. dev@workstation[:port]")
		portsCSV  = fs.String("ports", "", "comma-separated ports to forward (e.g. 3000,5173)")
		autoDisc  = fs.Bool("auto-discover", false, "probe the remote for common dev ports")
		cfgPath   = fs.String("config", "", "where to write the YAML (default: ./mole.yaml)")
		global    = fs.Bool("global", false, "write to the user-global config (~/.config/mole/config.yaml)")
		printOnly = fs.Bool("print", false, "print the generated YAML to stdout instead of writing a file")
		up        = fs.Bool("up", false, "start mole (mole up) immediately after writing the config")
		noPrompt  = fs.Bool("no-prompt", false, "don't ask questions; require all flags / env vars")
		yes       = fs.Bool("yes", false, "accept defaults for any unanswered questions")
		test      = fs.Bool("test", false, "after writing the config, test the SSH connection")
		force     = fs.Bool("force", false, "overwrite the config file if it already exists")
		clip      = fs.Bool("clip", false, "configure clip_* keys so `mole clip` works (skip the interactive prompt)")
		clipURL   = fs.String("clip-url", "", "URL the LXC pulls from (only used with -clip); e.g. http://10.0.0.1:7777")
		clipListen = fs.String("clip-listen", "", "address the Mac serves on (only used with -clip; defaults to the clip URL or 127.0.0.1:7777)")
		clipInterval = fs.Int("clip-interval-ms", 500, "clipboard poll cadence in ms (only used with -clip)")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole init [flags]

Generates a mole.yaml by asking a few questions (or reading the answers
from flags / env vars when -no-prompt is set).

Flags:
  -remote <target>   SSH target, e.g. dev@workstation[:port]
  -ports <list>      comma-separated ports to forward (e.g. 3000,5173)
  -auto-discover     probe the remote for common dev ports
  -config <path>     where to write the YAML (default: ./mole.yaml)
  -global            write to the user-global config (~/.config/mole/...)
  -print             print the generated YAML to stdout instead of writing
  -no-prompt         don't ask; require all values via flags / env vars
  -yes               accept defaults for any unanswered questions
  -test              after writing, test the SSH connection
  -force             overwrite the config file if it already exists
  -up                start mole (mole up) immediately after writing
  -clip              configure clipboard sharing options
  -clip-url <url>    URL the remote LXC pulls clipboard images from
  -clip-listen <addr> address the local clipboard server binds on
  -clip-interval-ms <n> clipboard polling interval in milliseconds
  -h, --help         show this help

Environment (read when the corresponding flag is empty):
  MOLE_REMOTE, MOLE_PORTS, MOLE_AUTO_DISCOVER,
  MOLE_CONFIG_PATH, MOLE_GLOBAL, MOLE_CLIP,
  MOLE_CLIP_URL, MOLE_CLIP_LISTEN`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	interactive := !*noPrompt
	if interactive && !isTerminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "error: stdin is not a TTY. Re-run with -no-prompt and pass the values via flags or env vars.")
		return 2
	}

	ans, err := gatherAnswers(initInputs{
		Remote:    *remote,
		PortsCSV:  *portsCSV,
		AutoDisc:  *autoDisc,
		Config:    *cfgPath,
		Global:    *global,
		PrintOnly: *printOnly,
		ClipEnabled: *clip,
		ClipURL:   *clipURL,
		ClipListen: *clipListen,
		ClipIntervalMs: *clipInterval,
	}, initOptions{
		Interactive: interactive,
		Yes:         *yes,
		In:          os.Stdin,
		Out:         os.Stderr, // prompts go to stderr so -print output is clean
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		return 1
	}

	yaml := renderYAML(ans)

	if ans.PrintOnly {
		fmt.Print(yaml)
		return 0
	}

	// Resolve final destination (and refuse to clobber without -force).
	dest := ans.ConfigPath
	if dest == "" {
		dest = "./mole.yaml"
	}
	if exists(dest) && !*force && interactive {
		fmt.Fprintf(os.Stderr, "%s already exists. Overwrite? [y/N]: ", dest)
		ok := readLine(os.Stdin)
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ok)), "y") {
			fmt.Fprintln(os.Stderr, "aborted: existing config left untouched")
			return 0
		}
	} else if exists(dest) && !*force {
		fmt.Fprintf(os.Stderr, "error: %s already exists (pass -force to overwrite)\n", dest)
		return 1
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		return 1
	}
	if err := os.WriteFile(dest, []byte(yaml), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "init error:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", dest)

	if *test {
		testRemote(ans.Remote)
	}

	// Optionally start mole right away. Honour -up unconditionally; in
	// interactive mode without -up, offer it (default yes). PrintOnly
	// has no file to start from, so skip.
	startNow := *up
	if !startNow && interactive && !ans.PrintOnly {
		fmt.Fprint(os.Stderr, "Start mole now? [Y/n]: ")
		a := strings.ToLower(strings.TrimSpace(readLine(os.Stdin)))
		startNow = a == "" || strings.HasPrefix(a, "y")
	}
	if startNow && !ans.PrintOnly {
		// Start in the background so the shell returns immediately;
		// stop later with `mole down`.
		fmt.Fprintln(os.Stderr, "starting mole in the background")
		return runUp([]string{"-config", dest, "-d"})
	}

	return 0
}

type initInputs struct {
	Remote    string
	PortsCSV  string
	AutoDisc  bool
	Config    string
	Global    bool
	PrintOnly bool
	ClipEnabled bool
	ClipURL string
	ClipListen string
	ClipIntervalMs int
}

type initOptions struct {
	Interactive bool
	Yes         bool
	In          io.Reader
	Out         io.Writer
}

func envDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string) (bool, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return false, false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false
	}
	return b, true
}

func gatherAnswers(in initInputs, opt initOptions) (*initAnswers, error) {
	// Allow env-var fallback when a flag is empty.
	if in.Remote == "" {
		in.Remote = envDefault("MOLE_REMOTE", "")
	}
	if in.PortsCSV == "" {
		in.PortsCSV = envDefault("MOLE_PORTS", "")
	}
	if !in.AutoDisc {
		if b, ok := envBool("MOLE_AUTO_DISCOVER"); ok {
			in.AutoDisc = b
		}
	}
	if in.Config == "" {
		in.Config = envDefault("MOLE_CONFIG_PATH", "")
	}
	if !in.Global {
		if b, ok := envBool("MOLE_GLOBAL"); ok {
			in.Global = b
		}
	}
	if !in.ClipEnabled {
		if b, ok := envBool("MOLE_CLIP"); ok {
			in.ClipEnabled = b
		}
	}
	if in.ClipURL == "" {
		in.ClipURL = envDefault("MOLE_CLIP_URL", "")
	}
	if in.ClipListen == "" {
		in.ClipListen = envDefault("MOLE_CLIP_LISTEN", "")
	}
	clipListenProvided := strings.TrimSpace(in.ClipListen) != ""
	if in.ClipListen == "" {
		in.ClipListen = config.DefaultClipListen
	}

	ans := &initAnswers{
		Remote:       strings.TrimSpace(in.Remote),
		AutoDiscover: in.AutoDisc,
		PrintOnly:    in.PrintOnly,
		Global:       in.Global,
		ConfigPath:   strings.TrimSpace(in.Config),
		ClipEnabled:  in.ClipEnabled,
		ClipURL:      strings.TrimSpace(in.ClipURL),
		ClipListen:   strings.TrimSpace(in.ClipListen),
		ClipIntervalMs: in.ClipIntervalMs,
	}
	ans.Ports = config.ParsePorts(in.PortsCSV)
	if ans.ClipEnabled && ans.ClipURL != "" {
		listen, err := clipListenForURL(ans.ClipURL)
		if err != nil {
			return nil, err
		}
		if !clipListenProvided {
			ans.ClipListen = listen
		}
	}

	// In non-interactive mode, all required values must already be set.
	if !opt.Interactive {
		if ans.Remote == "" {
			return nil, errors.New("-remote is required (or set MOLE_REMOTE)")
		}
		if err := validateRemote(ans.Remote); err != nil {
			return nil, err
		}
		if !ans.AutoDiscover && len(ans.Ports) == 0 {
			return nil, errors.New("either -ports or -auto-discover is required (or set MOLE_PORTS / MOLE_AUTO_DISCOVER)")
		}
		// Default config path if the user didn't pick one.
		if ans.ConfigPath == "" {
			if ans.Global {
				ans.ConfigPath = config.GlobalPath()
			} else {
				ans.ConfigPath = "./mole.yaml"
			}
		}
		return ans, nil
	}

	// Interactive: prompt for anything still missing.
	fmt.Fprintln(opt.Out, "configuring mole — press Enter to accept the default in [brackets]")
	for {
		raw := prompt(opt.In, opt.Out, "SSH remote (user@host[:port] or ssh config alias)", ans.Remote)
		if raw == "" && !opt.Yes {
			fmt.Fprintln(opt.Out, "  remote is required. Examples: dev@workstation, deploy@1.2.3.4:2222, or an ssh config Host alias like 'dev'")
			continue
		}
		if err := validateRemote(raw); err != nil {
			fmt.Fprintf(opt.Out, "  %v\n", err)
			if opt.Yes {
				return nil, err
			}
			continue
		}
		ans.Remote = raw
		break
	}

	// Port choice: 1) auto-discover  2) explicit  3) skip
	current := "1"
	if !ans.AutoDiscover && len(ans.Ports) > 0 {
		current = "2"
	} else if !ans.AutoDiscover && len(ans.Ports) == 0 && in.PortsCSV == "" && !in.AutoDisc {
		current = "1"
	}
	choice := promptChoice(opt.In, opt.Out,
		"How should mole pick ports?",
		[]string{
			"auto-discover common dev ports (recommended)",
			"explicit list (comma-separated)",
			"skip — I'll configure ports later",
		}, current)
	switch choice {
	case "1":
		ans.AutoDiscover = true
		ans.Ports = nil
	case "2":
		ans.AutoDiscover = false
		defaultPorts := "3000,5173,8080"
		if in.PortsCSV != "" {
			defaultPorts = in.PortsCSV
		}
		for {
			raw := prompt(opt.In, opt.Out, "Ports to forward", defaultPorts)
			ports := config.ParsePorts(raw)
			if len(ports) == 0 {
				fmt.Fprintln(opt.Out, "  expected comma-separated integers, got:", raw)
				if opt.Yes {
					return nil, fmt.Errorf("invalid ports: %q", raw)
				}
				continue
			}
			ans.Ports = ports
			break
		}
	default:
		ans.AutoDiscover = false
		ans.Ports = nil
	}

	// Clipboard sharing: ask once whether to wire it up. Default is
	// "no" so an unsuspecting user doesn't open a port they don't
	// need. We only ask in interactive mode; -clip / -clip-url on
	// the command line skip the question entirely.
	if !ans.ClipEnabled && in.ClipURL == "" {
		raw := prompt(opt.In, opt.Out, "Sync clipboard screenshots over Tailscale/WireGuard? [y/N]", "n")
		ans.ClipEnabled = strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "y")
		if ans.ClipEnabled {
			// Ask for the URL the LXC will pull from. We default to
			// 100.64.0.10:7777 (a representative Tailscale address),
			// but the user almost certainly needs to override this with
			// their actual Tailscale or WireGuard IP.
			urlRaw := prompt(opt.In, opt.Out, "Mac private IP the LXC will pull from (e.g. 100.64.0.10:7777)", "100.64.0.10:7777")
			if !strings.HasPrefix(urlRaw, "http://") && !strings.HasPrefix(urlRaw, "https://") {
				urlRaw = "http://" + urlRaw
			}
			ans.ClipURL = urlRaw
			listenDefault, err := clipListenForURL(ans.ClipURL)
			if err != nil {
				return nil, err
			}
			if clipListenProvided {
				listenDefault = ans.ClipListen
			}
			ans.ClipListen = prompt(opt.In, opt.Out, "Address mole clip serve binds on the Mac (use a private Tailscale/WireGuard IP for remote access)", listenDefault)
			intervalRaw := prompt(opt.In, opt.Out, "Clipboard poll interval (ms)", "500")
			if n, perr := strconv.Atoi(strings.TrimSpace(intervalRaw)); perr == nil && n > 0 {
				ans.ClipIntervalMs = n
			} else {
				ans.ClipIntervalMs = 500
			}
		}
	}

	// Save location. If the user already pinned a path via -config /
	// -global / env var, respect that and don't ask.
	if ans.ConfigPath == "" {
		defaultChoice := "1"
		if ans.Global {
			defaultChoice = "2"
		} else if ans.PrintOnly {
			defaultChoice = "3"
		}
		choice = promptChoice(opt.In, opt.Out,
			"Where to save the config?",
			[]string{
				"./mole.yaml               (current directory, project-local)",
				config.GlobalPath() + "  (user-global)",
				"don't save — print to stdout instead",
			}, defaultChoice)
		switch choice {
		case "1":
			ans.Global = false
			ans.PrintOnly = false
			ans.ConfigPath = "./mole.yaml"
		case "2":
			ans.Global = true
			ans.PrintOnly = false
			ans.ConfigPath = config.GlobalPath()
		default:
			ans.PrintOnly = true
			ans.ConfigPath = ""
		}
	} else if ans.Global {
		// -global was set but a custom -config also came through;
		// honour the more specific one.
		ans.Global = false
	}

	return ans, nil
}

// ---------------------------------------------------------------------------
// YAML rendering
// ---------------------------------------------------------------------------

func renderYAML(ans *initAnswers) string {
	var b strings.Builder
	b.WriteString("# mole — generated by `mole init` on ")
	b.WriteString(time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	b.WriteString("\n# See https://github.com/Luqueee/mole for the full reference.\n\n")
	b.WriteString("remote: ")
	b.WriteString(ans.Remote)
	b.WriteString("\n")
	if ans.AutoDiscover {
		b.WriteString("auto_discover: true\n")
		b.WriteString("\n# Ports never auto-forwarded (system/reserved). Uncomment to\n")
		b.WriteString("# override the default [22, 25, 53, 111, 631]; [] excludes nothing.\n")
		b.WriteString("# exclude_ports: [22, 25, 53, 111, 631]\n")
	}
	if len(ans.Ports) > 0 {
		b.WriteString("ports:\n")
		for _, p := range ans.Ports {
			b.WriteString("  - ")
			b.WriteString(strconv.Itoa(p))
			b.WriteString("\n")
		}
	}
	if ans.ClipEnabled {
		b.WriteString("\n# Clipboard sharing over a private WireGuard or Tailscale link. `mole clip serve`\n")
		b.WriteString("# on the Mac binds clip_listen; `mole clip pull` on the remote\n")
		b.WriteString("# reaches clip_url. Both commands auto-read these keys.\n")
		b.WriteString("clip_url: ")
		b.WriteString(ans.ClipURL)
		b.WriteString("\nclip_listen: ")
		b.WriteString(ans.ClipListen)
		b.WriteString("\nclip_interval_ms: ")
		b.WriteString(strconv.Itoa(ans.ClipIntervalMs))
		b.WriteString("\n")
	}
	b.WriteString("\n# Admin HTTP API (set to \"\" to disable).\n")
	b.WriteString("admin_addr: 127.0.0.1:9999\n\n")
	b.WriteString("log_level: info\n")
	b.WriteString("ssh_port: 22\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func clipListenForURL(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return config.DefaultClipListen, nil
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("invalid clip URL: malformed syntax or port")
	}
	if parsed.Hostname() == "" {
		return "", errors.New("invalid clip URL: missing host")
	}
	port := parsed.Port()
	if port == "" {
		port = "7777"
	}
	return net.JoinHostPort(parsed.Hostname(), port), nil
}

func validateRemote(r string) error {
	r = strings.TrimSpace(r)
	if r == "" {
		return errors.New("remote is required")
	}
	// A spec with '@' must be a well-formed user@host[:port].
	if strings.Contains(r, "@") {
		at := strings.LastIndex(r, "@")
		if at <= 0 || at == len(r)-1 {
			return fmt.Errorf("invalid remote %q: must be user@host[:port]", r)
		}
		// Reject a second '@' to catch typos.
		if strings.Count(r, "@") != 1 {
			return fmt.Errorf("invalid remote %q: must be user@host[:port]", r)
		}
		return nil
	}
	// Otherwise it's an ssh_config Host alias, resolved at connect time
	// via `ssh -G`. Only reject obviously-malformed input (whitespace).
	if strings.ContainsAny(r, " \t") {
		return fmt.Errorf("invalid remote %q: use user@host[:port] or an ssh config Host alias", r)
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readLine(r io.Reader) string {
	sc := bufio.NewScanner(r)
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}

// isTerminal reports whether f is a terminal. On Windows we fall back
// to a stat-based check because golang.org/x/term isn't in the
// module's deps and we don't want to add a dependency just for this.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// prompt prints "[default]: " after the prompt and returns the user's
// response, or the default if the response is empty. Errors from the
// reader (closed stdin) are treated as "use default".
func prompt(in io.Reader, out io.Writer, label, def string) string {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line := readLine(in)
	if strings.TrimSpace(line) == "" {
		return def
	}
	return strings.TrimSpace(line)
}

// promptChoice prints a numbered list, prompts for a number, and
// returns the chosen index as a string. Returns the default for any
// non-numeric or out-of-range input.
func promptChoice(in io.Reader, out io.Writer, label string, options []string, def string) string {
	fmt.Fprintln(out, label)
	for i, o := range options {
		fmt.Fprintf(out, "  %d) %s\n", i+1, o)
	}
	fmt.Fprintf(out, "  choose [%s]: ", def)
	line := strings.TrimSpace(readLine(in))
	if line == "" {
		return def
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(options) {
		return def
	}
	return strconv.Itoa(n)
}

// testRemote does a best-effort SSH handshake using the system `ssh`
// binary. It uses BatchMode=yes so a missing key produces a clean
// error rather than a password prompt.
func testRemote(remote string) {
	if err := validateRemote(remote); err != nil {
		fmt.Fprintln(os.Stderr, "test:", err)
		return
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test: 'ssh' not on PATH; skipping connectivity check")
		return
	}
	fmt.Fprintf(os.Stderr, "testing ssh %s (BatchMode, 5s timeout) ...\n", remote)
	// Use the user's existing config: StrictHostKeyChecking=accept-new
	// adds unknown hosts without prompting, matching `mole`'s default.
	cmd := exec.Command(ssh,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=accept-new",
		remote, "true")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "test: ssh exited with code %d (this is OK if you use password auth or a custom key path)\n", exitErr.ExitCode())
			return
		}
		// E.g. network-level failure.
		var netErr net.Error
		if errors.As(err, &netErr) {
			fmt.Fprintf(os.Stderr, "test: ssh network error: %v\n", netErr)
			return
		}
		fmt.Fprintf(os.Stderr, "test: ssh failed: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "test: ssh handshake succeeded")
}
