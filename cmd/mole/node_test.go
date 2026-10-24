package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Luqueee/mole/internal/agent"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/tunnel"
)

func TestSplitNodeFlag(t *testing.T) {
	cases := []struct {
		in   []string
		node string
		rest []string
		ok   bool
		err  bool
	}{
		{[]string{"-n", "devlabs", "ports", "add", "3000"}, "devlabs", []string{"ports", "add", "3000"}, true, false},
		{[]string{"--node", "devlabs", "status"}, "devlabs", []string{"status"}, true, false},
		{[]string{"-n=devlabs", "status"}, "devlabs", []string{"status"}, true, false},
		{[]string{"--node=devlabs"}, "devlabs", []string{}, true, false},
		{[]string{"-n"}, "", nil, true, true},
		{[]string{"status", "-n", "x"}, "", nil, false, false}, // -n only counts first
		{[]string{"up", "-d"}, "", nil, false, false},
		{nil, "", nil, false, false},
	}
	for _, c := range cases {
		node, rest, ok, err := splitNodeFlag(c.in)
		if ok != c.ok || (err != nil) != c.err || node != c.node || len(rest) != len(c.rest) {
			t.Errorf("splitNodeFlag(%q) = %q %q %v %v", c.in, node, rest, ok, err)
		}
	}
}

// The quoting is what stands between a hostile argument and a shell on the
// node, so it is checked with a real shell: a stand-in `mole` prints the
// arguments it receives, one per line.
func TestRemoteCommandSurvivesARealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"
	if err := os.WriteFile(filepath.Join(dir, "mole"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"ports", "add", "3000",
		"has space", "it's", `"double"`, "$HOME", "`id`", "$(id)", "a;b", "a|b", "a&b", "a>b", "*", "~", "-n", "--flag=va lue",
		"ünïcode ñ", "", "tab\there",
	}
	// An absolute bin: remoteEnv puts /usr/local/bin and friends ahead of
	// PATH, and a real mole may live there.
	standIn := filepath.Join(dir, "mole")
	cmd := exec.Command("sh", "-c", remoteCommand(standIn, args...))
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != len(args) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(args), out)
	}
	for i, a := range args {
		if got[i] != a {
			t.Errorf("arg %d: got %q, want %q", i, got[i], a)
		}
	}

	// A newline inside an argument must stay inside it.
	cmd = exec.Command("sh", "-c", remoteCommand(standIn, "line1\nline2"))
	out, _ = cmd.Output()
	if string(out) != "line1\nline2\n" {
		t.Errorf("newline argument: %q", out)
	}
}

func TestRemoteCommandShape(t *testing.T) {
	if got := remoteCommand("mole", "agent", "--stdio"); got != remoteEnv+"mole agent --stdio" {
		t.Errorf("plain command: %q", got)
	}
	if got := remoteCommand("~/bin/mole", "version"); !strings.Contains(got, `"$HOME"/bin/mole version`) {
		t.Errorf("tilde bin: %q", got)
	}
	if got := remoteCommand("/opt/my tools/mole", "x"); !strings.Contains(got, `'/opt/my tools/mole' x`) {
		t.Errorf("bin with a space: %q", got)
	}
}

func TestParseVersionOutput(t *testing.T) {
	for in, want := range map[string]string{
		"mole v0.3.0\n": "0.3.0",
		"\x1b[1m\x1b[38;2;1;2;3mmole\x1b[0m \x1b[2mv0.1.1\x1b[0m\n": "0.1.1",
		"bash: mole: command not found":                             "",
		"":                                                          "",
	} {
		if got := parseVersionOutput(in); got != want {
			t.Errorf("parseVersionOutput(%q) = %q, want %q", in, got, want)
		}
	}
}

// meshNode is a registered node backed by the in-process sshd.
type meshNode struct {
	t    *testing.T
	srv  *sshTestServer
	cfg  string // path of the config that registers the node
	port int
	mu   sync.Mutex
	cmds []string
}

func (m *meshNode) commands() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.cmds...)
}

// newMeshNode starts an sshd whose exec handler is h, points HOME and the
// config at a scratch area, and registers the node as "devlabs".
func newMeshNode(t *testing.T, h func(cmd string, ch ssh.Channel) uint32) *meshNode {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("APPDATA", filepath.Join(home, "config"))
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Chdir(home) // no ./mole.yaml

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	m := &meshNode{t: t}
	m.srv = newSSHTestServer(t, signer.PublicKey())
	m.srv.exec = func(cmd string, ch ssh.Channel) uint32 {
		m.mu.Lock()
		m.cmds = append(m.cmds, cmd)
		m.mu.Unlock()
		return h(cmd, ch)
	}
	m.port = m.srv.ln.Addr().(*net.TCPAddr).Port
	m.cfg = filepath.Join(home, "config", "mole", "config.yaml")
	m.writeConfig(fmt.Sprintf("nodes:\n  devlabs:\n    remote: u@127.0.0.1:%d\n    insecure: true\n", m.port))
	return m
}

func (m *meshNode) writeConfig(body string) {
	m.t.Helper()
	if err := os.MkdirAll(filepath.Dir(m.cfg), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(m.cfg, []byte(body), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

// capture runs fn with os.Stdout and os.Stderr redirected, and returns what it wrote.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	oldO, oldE := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wo, we
	var wg sync.WaitGroup
	var bo, be bytes.Buffer
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(&bo, ro) }()
	go func() { defer wg.Done(); io.Copy(&be, re) }()
	func() {
		defer func() { os.Stdout, os.Stderr = oldO, oldE; wo.Close(); we.Close() }()
		fn()
	}()
	wg.Wait()
	return bo.String(), be.String()
}

func agentHandler(cmd string, ch ssh.Channel) uint32 {
	if cmd == remoteCommand("mole", "agent", "--stdio") {
		if err := agent.Serve(context.Background(), ch, nodeAgent{}); err != nil {
			fmt.Fprintln(ch.Stderr(), "agent:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(ch.Stderr(), "unexpected command %q\n", cmd)
	return 127
}

func TestProbeNodeWithAgent(t *testing.T) {
	m := newMeshNode(t, agentHandler)
	cfg, err := config.Load(m.cfg)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := cfg.Node("devlabs")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := probeNode(ctx, cfg, n)
	if p.Err != nil || !p.HasAgent {
		t.Fatalf("probe: %+v", p)
	}
	if p.Hello.Version != version || p.Hello.OS != runtime.GOOS || p.Hello.Agent != agent.ProtocolVersion {
		t.Fatalf("hello: %+v", p.Hello)
	}
	if p.Status.Running || p.Status.Nodes != 1 {
		t.Fatalf("status (the test node has no daemon): %+v", p.Status)
	}
	// Not "> 0": Windows' clock is coarse enough to measure a fast round trip as 0.
	if p.Connect < 0 || p.RoundTrip < 0 {
		t.Fatalf("timings: %v %v", p.Connect, p.RoundTrip)
	}
}

func TestProbeNodeWithoutAgent(t *testing.T) {
	// A mole that predates the agent: the error and its usage banner, then `version` works.
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 {
		switch cmd {
		case remoteCommand("mole", "agent", "--stdio"):
			fmt.Fprintln(ch.Stderr(), "✗ unknown command: agent")
			fmt.Fprint(ch, "\n  mole  v0.1.1\n  usage...\n")
			return 1
		case remoteCommand("mole", "version"):
			fmt.Fprintln(ch, "mole v0.1.1")
			return 0
		}
		return 127
	})
	cfg, _ := config.Load(m.cfg)
	n, _ := cfg.Node("devlabs")
	p := probeNode(context.Background(), cfg, n)
	if p.Err != nil || p.HasAgent || p.Version != "0.1.1" {
		t.Fatalf("probe: %+v", p)
	}
}

func TestProbeNodeWithoutMole(t *testing.T) {
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 {
		fmt.Fprintln(ch.Stderr(), "bash: line 1: mole: command not found")
		return 127
	})
	cfg, _ := config.Load(m.cfg)
	n, _ := cfg.Node("devlabs")
	p := probeNode(context.Background(), cfg, n)
	if p.Err == nil || !strings.Contains(p.Err.Error(), "not installed") {
		t.Fatalf("probe: %+v", p)
	}
}

func TestProbeNodeUnreachable(t *testing.T) {
	m := newMeshNode(t, agentHandler)
	cfg, _ := config.Load(m.cfg)
	n, _ := cfg.Node("devlabs")
	m.srv.ln.Close()
	m.srv.kill()
	p := probeNode(context.Background(), cfg, n)
	if p.Err == nil {
		t.Fatalf("an unreachable node reported as reachable: %+v", p)
	}
}

func TestRunOnNodeRelaysOutputAndExitCode(t *testing.T) {
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 {
		fmt.Fprintln(ch, "to stdout")
		fmt.Fprintln(ch.Stderr(), "to stderr")
		return 3
	})
	var code int
	out, errOut := capture(t, func() { code = runOnNode("devlabs", []string{"ports", "add", "a b", "$HOME"}) })
	if code != 3 {
		t.Fatalf("exit code %d, want the node's 3", code)
	}
	if out != "to stdout\n" || !strings.Contains(errOut, "to stderr") {
		t.Fatalf("stdout=%q stderr=%q", out, errOut)
	}
	want := remoteCommand("mole", "ports", "add", "a b", "$HOME")
	if cmds := m.commands(); len(cmds) != 1 || cmds[0] != want {
		t.Fatalf("the node was asked %q, want %q", cmds, want)
	}
}

func TestRunOnNodeUsesTheNodesBinary(t *testing.T) {
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 { return 0 })
	m.writeConfig(fmt.Sprintf("nodes:\n  devlabs:\n    remote: u@127.0.0.1:%d\n    insecure: true\n    bin: ~/apps/mole\n", m.port))
	capture(t, func() { runOnNode("devlabs", []string{"version"}) })
	if cmds := m.commands(); len(cmds) != 1 || cmds[0] != remoteCommand("~/apps/mole", "version") {
		t.Fatalf("commands: %q", cmds)
	}
}

func TestRunOnNodeErrors(t *testing.T) {
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 { return 0 })
	var code int
	_, errOut := capture(t, func() { code = runOnNode("nope", []string{"status"}) })
	if code == 0 || !strings.Contains(errOut, `unknown node "nope"`) || !strings.Contains(errOut, "devlabs") {
		t.Fatalf("unknown node: code=%d stderr=%q", code, errOut)
	}
	_, errOut = capture(t, func() { code = runOnNode("devlabs", nil) })
	if code != 2 || !strings.Contains(errOut, "Usage") {
		t.Fatalf("no command: code=%d stderr=%q", code, errOut)
	}
	if len(m.commands()) != 0 {
		t.Fatalf("a command reached the node: %q", m.commands())
	}
	// An unreachable node is an error, not a hang.
	m.srv.ln.Close()
	m.srv.kill()
	_, errOut = capture(t, func() { code = runOnNode("devlabs", []string{"status"}) })
	if code != 255 || !strings.Contains(errOut, "cannot reach node devlabs") {
		t.Fatalf("unreachable: code=%d stderr=%q", code, errOut)
	}
}

func TestRunOnNodeResolvesTheConfigsOwnRemote(t *testing.T) {
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 { fmt.Fprintln(ch, "hi"); return 0 })
	target := fmt.Sprintf("u@127.0.0.1:%d", m.port)
	m.writeConfig("remote: " + target + "\ninsecure: true\n")
	var code int
	out, _ := capture(t, func() { code = runOnNode(target, []string{"status"}) })
	if code != 0 || out != "hi\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func mgrFor(t *testing.T, m *meshNode) *tunnel.Manager {
	t.Helper()
	cfg, _ := config.Load(m.cfg)
	n, _ := cfg.Node("devlabs")
	mgr, err := dialNode(cfg, n, newLogger("error"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Close() })
	return mgr
}

func TestExecStdinPtyResizeAndInterrupt(t *testing.T) {
	var started sync.WaitGroup
	started.Add(1)
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 {
		switch cmd {
		case "echo-upper":
			b, _ := io.ReadAll(ch)
			ch.Write(bytes.ToUpper(b))
			return 0
		case "block":
			started.Done()
			io.Copy(io.Discard, ch) // until the session is closed
			return 0
		}
		return 127
	})
	mgr := mgrFor(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// stdin reaches the command and its output comes back
	var out bytes.Buffer
	code, err := mgr.Exec(ctx, "echo-upper", tunnel.ExecOptions{Stdin: strings.NewReader("hello node"), Stdout: &out})
	if err != nil || code != 0 || out.String() != "HELLO NODE" {
		t.Fatalf("stdin: code=%d err=%v out=%q", code, err, out.String())
	}

	// a terminal is requested with the right size, and resizes are forwarded
	resize := make(chan tunnel.Size, 1)
	runCtx, stop := context.WithCancel(ctx)
	keyboard, typed := io.Pipe() // a terminal's input never ends on its own
	defer typed.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.Exec(runCtx, "block", tunnel.ExecOptions{
			Stdin: keyboard, Stdout: io.Discard,
			PTY:    &tunnel.PTYOptions{Term: "xterm-test", Size: tunnel.Size{Rows: 24, Cols: 80}},
			Resize: resize,
		})
	}()
	started.Wait()
	resize <- tunnel.Size{Rows: 40, Cols: 120}
	eventually(t, "the terminal request", func() bool { return m.srv.sawEvent("pty xterm-test 80x24") })
	eventually(t, "the resize", func() bool { return m.srv.sawEvent("winch 120x40") })

	// cancelling interrupts the command, then closes the session if it lingers
	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Exec ignored cancellation")
	}
	if !m.srv.sawEvent("signal INT") {
		t.Fatal("the node was not sent an interrupt")
	}
}

func TestNodeAddListRemoveEndToEnd(t *testing.T) {
	m := newMeshNode(t, agentHandler)
	m.writeConfig("# my nodes\nssh_port: 22\n")

	target := fmt.Sprintf("u@127.0.0.1:%d", m.port)
	var code int
	out, errOut := capture(t, func() {
		code = runNode([]string{"add", "devlabs", target, "-insecure", "-config", m.cfg})
	})
	if code != 0 || !strings.Contains(out, "devlabs: mole "+version) || !strings.Contains(out, "mole -n devlabs") {
		t.Fatalf("add: code=%d out=%q err=%q", code, out, errOut)
	}
	raw, _ := os.ReadFile(m.cfg)
	if !strings.Contains(string(raw), "# my nodes") || !strings.Contains(string(raw), "devlabs:") || !strings.Contains(string(raw), "insecure: true") {
		t.Fatalf("config after add:\n%s", raw)
	}

	// adding it again is refused; a bad name is refused
	_, errOut = capture(t, func() { code = runNode([]string{"add", "devlabs", target, "-config", m.cfg}) })
	if code == 0 || !strings.Contains(errOut, "already registered") {
		t.Fatalf("duplicate add: code=%d %q", code, errOut)
	}
	_, errOut = capture(t, func() { code = runNode([]string{"add", "Bad Name", target, "-config", m.cfg}) })
	if code != 2 {
		t.Fatalf("bad name: code=%d %q", code, errOut)
	}

	out, _ = capture(t, func() { code = runNode([]string{"ls", "-config", m.cfg}) })
	if code != 0 || !strings.Contains(out, "devlabs") || !strings.Contains(out, "ready") || !strings.Contains(out, version) || !strings.Contains(out, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("ls: code=%d\n%s", code, out)
	}
	out, _ = capture(t, func() { code = runNode([]string{"ping", "devlabs", "-config", m.cfg}) })
	if code != 0 || !strings.Contains(out, "devlabs: connect") {
		t.Fatalf("ping: code=%d %q", code, out)
	}

	// an unreachable node is refused unless told not to check, and listed as such
	_, errOut = capture(t, func() { code = runNode([]string{"add", "ghost", "u@127.0.0.1:1", "-insecure", "-config", m.cfg}) })
	if code == 0 || !strings.Contains(errOut, "cannot reach") {
		t.Fatalf("unreachable add: code=%d %q", code, errOut)
	}
	capture(t, func() {
		code = runNode([]string{"add", "ghost", "u@127.0.0.1:1", "-insecure", "-no-check", "-config", m.cfg})
	})
	if code != 0 {
		t.Fatalf("add -no-check: %d", code)
	}
	out, _ = capture(t, func() { runNode([]string{"ls", "-config", m.cfg}) })
	if !strings.Contains(out, "ghost") || !strings.Contains(out, "unreachable") || !strings.Contains(out, "ready") {
		t.Fatalf("ls with an unreachable node:\n%s", out)
	}

	capture(t, func() { code = runNode([]string{"rm", "ghost", "-config", m.cfg}) })
	capture(t, func() { code = runNode([]string{"rm", "devlabs", "-config", m.cfg}) })
	_, errOut = capture(t, func() { code = runNode([]string{"rm", "devlabs", "-config", m.cfg}) })
	if code == 0 || !strings.Contains(errOut, "not registered") {
		t.Fatalf("second rm: code=%d %q", code, errOut)
	}
	cfg, err := config.Load(m.cfg)
	if err != nil || len(cfg.Nodes) != 0 {
		t.Fatalf("nodes left: %+v %v", cfg.Nodes, err)
	}
	out, _ = capture(t, func() { runNode([]string{"ls", "-config", m.cfg}) })
	if !strings.Contains(out, "no nodes yet") {
		t.Fatalf("empty ls: %q", out)
	}
}

func TestNodeCommandWithoutAgentIsStillRegistered(t *testing.T) {
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 {
		if cmd == remoteCommand("mole", "version") {
			fmt.Fprintln(ch, "mole v0.1.1")
			return 0
		}
		fmt.Fprintln(ch.Stderr(), "unknown command")
		return 1
	})
	m.writeConfig("")
	var code int
	out, _ := capture(t, func() {
		code = runNode([]string{"add", "old", fmt.Sprintf("u@127.0.0.1:%d", m.port), "-insecure", "-config", m.cfg})
	})
	if code != 0 || !strings.Contains(out, "has no agent") || !strings.Contains(out, "0.1.1") {
		t.Fatalf("add old node: code=%d %q", code, out)
	}
	out, _ = capture(t, func() { runNode([]string{"ls", "-config", m.cfg}) })
	if !strings.Contains(out, "no agent") || !strings.Contains(out, "0.1.1") {
		t.Fatalf("ls: %q", out)
	}
}

func TestNodeAgentStatusSeesTheLocalDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("APPDATA", filepath.Join(home, "config"))
	t.Chdir(home)
	st, err := nodeAgent{}.Status()
	if err != nil || st.Running || st.AdminAddr != "" {
		t.Fatalf("no config: %+v %v", st, err)
	}
	if st.Inbox == "" {
		t.Fatal("the inbox is always reported")
	}
}

func TestPadVisible(t *testing.T) {
	if got := padVisible("\x1b[32mok\x1b[0m", 5); got != "\x1b[32mok\x1b[0m   " {
		t.Errorf("colour codes counted: %q", got)
	}
	if got := padVisible("abcdef", 3); got != "abcdef" {
		t.Errorf("longer than width: %q", got)
	}
}
