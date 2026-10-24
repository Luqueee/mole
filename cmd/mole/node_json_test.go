package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"

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
