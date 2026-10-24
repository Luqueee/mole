package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Luqueee/mole/internal/config"
)

func TestNodePingJSON(t *testing.T) {
	m := newMeshNode(t, agentHandler)
	var code int
	out, errOut := capture(t, func() { code = runNode([]string{"ping", "-json", "devlabs", "-config", m.cfg}) })
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	var info nodeInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if info.Name != "devlabs" || info.State != "ready" || info.Version != version || info.OS != runtime.GOOS || info.Implicit {
		t.Fatalf("info: %+v", info)
	}
	if info.Daemon == nil || info.Daemon.Running || info.Daemon.Ports == nil || info.Daemon.Push == nil {
		t.Fatalf("daemon: %+v (lists must be [], not null)", info.Daemon)
	}
	if !strings.Contains(out, `"ports": []`) || !strings.Contains(out, `"push": []`) {
		t.Fatalf("empty lists must print as []:\n%s", out)
	}
}

func TestNodePingJSONUnreachableIsAnAnswer(t *testing.T) {
	m := newMeshNode(t, agentHandler)
	m.srv.ln.Close()
	m.srv.kill()
	var code int
	out, _ := capture(t, func() { code = runNode([]string{"ping", "-json", "devlabs", "-config", m.cfg}) })
	var info nodeInfo
	if code != 0 || json.Unmarshal([]byte(out), &info) != nil || info.State != "unreachable" || info.Error == "" || info.Daemon != nil {
		t.Fatalf("code=%d info=%+v out=%q", code, info, out)
	}
}

func TestNodePingJSONUnknownNodeFails(t *testing.T) {
	m := newMeshNode(t, agentHandler)
	var code int
	out, errOut := capture(t, func() { code = runNode([]string{"ping", "-json", "nope", "-config", m.cfg}) })
	if code == 0 || out != "" || !strings.Contains(errOut, `unknown node "nope"`) {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestNodeLsJSON(t *testing.T) {
	m := newMeshNode(t, agentHandler)
	var code int
	capture(t, func() {
		code = runNode([]string{"add", "ghost", "u@127.0.0.1:1", "-insecure", "-no-check", "-ssh-port", "2222", "-bin", "~/apps/mole", "-config", m.cfg})
	})
	if code != 0 {
		t.Fatalf("add: %d", code)
	}
	out, _ := capture(t, func() { code = runNode([]string{"ls", "-json", "-config", m.cfg}) })
	var infos []nodeInfo
	if err := json.Unmarshal([]byte(out), &infos); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v\n%s", code, err, out)
	}
	by := map[string]nodeInfo{}
	for _, i := range infos {
		by[i.Name] = i
	}
	if by["devlabs"].State != "ready" || by["ghost"].State != "unreachable" {
		t.Fatalf("states: %+v", by)
	}
	g := by["ghost"]
	if g.SSHPort != 2222 || g.Bin != "~/apps/mole" || !g.Insecure || g.Target != "u@127.0.0.1:1" {
		t.Fatalf("ghost keeps what it was registered with: %+v", g)
	}

	// nothing registered: an empty list, not null
	m.writeConfig("")
	out, _ = capture(t, func() { code = runNode([]string{"ls", "-json", "-config", m.cfg}) })
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty: %q", out)
	}
}

func TestNodeLsJSONMarksTheConfigsOwnRemote(t *testing.T) {
	m := newMeshNode(t, agentHandler)
	target := fmt.Sprintf("u@127.0.0.1:%d", m.port)
	m.writeConfig("remote: " + target + "\ninsecure: true\n")
	var code int
	out, _ := capture(t, func() { code = runNode([]string{"ls", "-json", "-config", m.cfg}) })
	var infos []nodeInfo
	if err := json.Unmarshal([]byte(out), &infos); err != nil || code != 0 || len(infos) != 1 {
		t.Fatalf("code=%d err=%v\n%s", code, err, out)
	}
	if !infos[0].Implicit || infos[0].Name != target || infos[0].State != "ready" {
		t.Fatalf("%+v", infos[0])
	}
}

func TestNodeAddForceReplaces(t *testing.T) {
	m := newMeshNode(t, agentHandler)
	target := fmt.Sprintf("u@127.0.0.1:%d", m.port)
	var code int
	_, errOut := capture(t, func() {
		code = runNode([]string{"add", "devlabs", target, "-insecure", "-no-check", "-config", m.cfg})
	})
	if code == 0 || !strings.Contains(errOut, "-force") {
		t.Fatalf("without -force: code=%d %q", code, errOut)
	}
	capture(t, func() {
		code = runNode([]string{"add", "devlabs", target, "-insecure", "-no-check", "-force", "-bin", "~/new/mole", "-config", m.cfg})
	})
	cfg, err := config.Load(m.cfg)
	n := cfg.Nodes["devlabs"]
	if code != 0 || err != nil || n.Bin != "~/new/mole" || len(cfg.Nodes) != 1 {
		t.Fatalf("code=%d err=%v nodes=%+v", code, err, cfg.Nodes)
	}
}

func TestNodeInfoReportsTheDaemon(t *testing.T) {
	admin := `{"stats":{"uptime":"3h2m","active_conns":2,"total_conns":50,"failed_dials":4},` +
		`"info":{"ports":[3000,5173],"push":[{"local":8080,"remote":80,"bind":"127.0.0.1","state":"active"}],"transfer":{"receive":true,"accept":"auto"}}}`
	p := probe{HasAgent: true}
	p.Status.Running = true
	p.Status.Admin = json.RawMessage(admin)
	p.Status.DiscoverPorts = []int{3000, 5173, 9229}
	d := buildNodeDaemon(p)
	if !d.Running || d.Uptime != "3h2m" || d.ActiveConns != 2 || d.TotalConns != 50 || d.FailedDials != 4 ||
		len(d.Ports) != 2 || len(d.Push) != 1 || d.Push[0].Remote != 80 || !d.Receive || d.Accept != "auto" || len(d.DiscoverPorts) != 3 {
		t.Fatalf("%+v", d)
	}
}

func TestNodeWithoutMoleIsNotInstalled(t *testing.T) {
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 {
		fmt.Fprintln(ch.Stderr(), "bash: line 1: mole: command not found")
		return 127
	})
	var code int
	out, _ := capture(t, func() { code = runNode([]string{"ping", "-json", "devlabs", "-config", m.cfg}) })
	var info nodeInfo
	if code != 0 || json.Unmarshal([]byte(out), &info) != nil || info.State != "not-installed" || !strings.Contains(info.Error, "node install") {
		t.Fatalf("code=%d info=%+v", code, info)
	}
	if info.ConnectMs <= 0 && info.Error == "" {
		t.Fatalf("the node answered, so its connect time is known: %+v", info)
	}
	// adding it with a check says how to go on
	target := fmt.Sprintf("u@127.0.0.1:%d", m.port)
	_, errOut := capture(t, func() { code = runNode([]string{"add", "fresh", target, "-insecure", "-config", m.cfg}) })
	if code == 0 || !strings.Contains(errOut, "mole node install fresh") {
		t.Fatalf("add: code=%d %q", code, errOut)
	}
}

func TestNodeInstallRunsTheInstallerOnTheNode(t *testing.T) {
	t.Setenv("MOLE_RAW_URL", "https://raw.example/mole")
	m := newMeshNode(t, func(cmd string, ch ssh.Channel) uint32 {
		fmt.Fprintln(ch, "building mole")
		if strings.Contains(cmd, "no-such") {
			fmt.Fprintln(ch.Stderr(), "error: ref not found")
			return 1
		}
		return 0
	})
	var code int
	out, _ := capture(t, func() { code = runNode([]string{"install", "devlabs", "-config", m.cfg}) })
	cmds := m.commands()
	if code != 0 || len(cmds) != 1 || !strings.Contains(out, "building mole") || !strings.Contains(out, "mole installed on devlabs") {
		t.Fatalf("code=%d out=%q cmds=%q", code, out, cmds)
	}
	if !strings.Contains(cmds[0], "https://raw.example/mole/main/scripts/install.sh") || strings.Contains(cmds[0], "MOLE_VERSION") {
		t.Fatalf("the node was told %q", cmds[0])
	}

	out, _ = capture(t, func() { code = runNode([]string{"install", "devlabs", "-branch", "feat/x", "-config", m.cfg}) })
	cmds = m.commands()
	if code != 0 || !strings.Contains(cmds[1], "refs/heads/feat/x/scripts/install.sh") || !strings.Contains(cmds[1], "MOLE_VERSION=feat/x ") {
		t.Fatalf("branch: code=%d cmds=%q", code, cmds)
	}

	// a failing installer is a failure, and a hostile branch never reaches the node
	_, errOut := capture(t, func() { code = runNode([]string{"install", "devlabs", "-branch", "no-such", "-config", m.cfg}) })
	if code != 1 || !strings.Contains(errOut, "installer failed") {
		t.Fatalf("failure: code=%d %q", code, errOut)
	}
	n := len(m.commands())
	_, _ = capture(t, func() { code = runNode([]string{"install", "devlabs", "-branch", "a;rm -rf x", "-config", m.cfg}) })
	if code != 2 || len(m.commands()) != n {
		t.Fatalf("hostile branch: code=%d commands=%d->%d", code, n, len(m.commands()))
	}
}

func TestInstallScriptRunsWithARealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	// a stand-in curl that "downloads" a script printing its environment
	curl := "#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -o) out=\"$2\"; shift;; esac; shift; done\nif [ -n \"$FAIL\" ]; then exit 22; fi\nprintf 'echo ran:$MOLE_VERSION\\n' > \"$out\"\n"
	os.WriteFile(dir+"/curl", []byte(curl), 0o755)
	run := func(env ...string) (string, error) {
		c := exec.Command("sh", "-c", installScript("feat/x"))
		c.Env = append([]string{"PATH=" + dir + ":/usr/bin:/bin"}, env...)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	out, err := run()
	if err != nil || strings.TrimSpace(out) != "ran:feat/x" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	// a failed download must fail, not run an empty script
	if out, err := run("FAIL=1"); err == nil {
		t.Fatalf("a failed download succeeded: %q", out)
	}
}
