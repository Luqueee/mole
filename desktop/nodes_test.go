package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stubMole puts a `mole` on PATH that runs script (a POSIX shell body) and
// points HOME and the config at a scratch area. Like the real CLI it answers
// `mole version` with its identity before anything else, which is how the app
// tells it from another program called mole; that probe never reaches script.
func stubMole(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mole"), []byte("#!/bin/sh\nif [ \"$#\" -eq 1 ] && [ \"$1\" = version ]; then echo 'mole v0.0.0-test'; exit 0; fi\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("APPDATA", filepath.Join(home, "config"))
	t.Chdir(home)
	nodesSupported.mu.Lock()
	nodesSupported.ok = false
	nodesSupported.mu.Unlock()
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "mole")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGetNodesListsTheRegistryAndTheOwnRemote(t *testing.T) {
	stubMole(t, `exit 0`)
	writeConfig(t, "remote: devlabs\nnodes:\n  zeta:\n    remote: u@z.example:2200\n    insecure: true\n  alpha:\n    remote: alpha-alias\n    ssh_port: 2222\n    bin: ~/apps/mole\n")
	l := NewMoleService().GetNodes()
	if !l.CLIReady || l.CLIError != "" {
		t.Fatalf("cli: %+v", l)
	}
	var names []string
	for _, n := range l.Nodes {
		names = append(names, n.Name)
	}
	if !reflect.DeepEqual(names, []string{"alpha", "zeta", "devlabs"}) {
		t.Fatalf("names: %v", names)
	}
	a, d := l.Nodes[0], l.Nodes[2]
	if a.SSHPort != 2222 || a.Bin != "~/apps/mole" || a.Implicit || !d.Implicit || d.Target != "devlabs" || !l.Nodes[1].Insecure {
		t.Fatalf("nodes: %+v", l.Nodes)
	}
	if l.Self.OS != runtime.GOOS || l.Self.Hostname == "" {
		t.Fatalf("self: %+v", l.Self)
	}
}

func TestGetNodesTellsWhenTheCLIPredatesNodes(t *testing.T) {
	stubMole(t, `echo "unknown command: node" >&2; exit 2`)
	writeConfig(t, "nodes:\n  a:\n    remote: x\n")
	l := NewMoleService().GetNodes()
	if l.CLIReady || !strings.Contains(l.CLIError, "predates nodes") || len(l.Nodes) != 1 {
		t.Fatalf("%+v", l)
	}
	info := NewMoleService().ProbeNode("a")
	if !info.CLIOutdated || info.State != "unreachable" {
		t.Fatalf("%+v", info)
	}
}

func TestProbeNodeReadsTheCLIsJSON(t *testing.T) {
	stubMole(t, `
case "$*" in
  "node help") exit 0 ;;
  "node ping -json devlabs") cat <<'JSON'
{"name":"devlabs","target":"u@h","state":"ready","version":"0.4.0","build":"main@abc1234","os":"linux","arch":"amd64","hostname":"dev","agent":1,"connectMs":41.5,"roundTripMs":3.2,
 "daemon":{"running":true,"uptime":"2h","activeConns":1,"totalConns":9,"failedDials":0,"ports":[3000],"push":null,"receive":true,"discoverPorts":null,"excludePorts":[22]}}
JSON
  ;;
  *) echo "unexpected: $*" >&2; exit 9 ;;
esac`)
	info := NewMoleService().ProbeNode("devlabs")
	if info.State != "ready" || info.Version != "0.4.0" || info.Build != "main@abc1234" || info.ConnectMs != 41.5 || info.Daemon == nil || !info.Daemon.Running {
		t.Fatalf("%+v", info)
	}
	if info.Daemon.Push == nil || info.Daemon.DiscoverPorts == nil || len(info.Daemon.ExcludePorts) != 1 {
		t.Fatalf("lists must be [] for the frontend, not null: %+v", info.Daemon)
	}
}

func TestProbeNodeRefusesFlagsAndReportsCLIErrors(t *testing.T) {
	stubMole(t, `
[ "$*" = "node help" ] && exit 0
echo 'mole node: unknown node "ghost": known nodes are a' >&2; exit 1`)
	m := NewMoleService()
	if i := m.ProbeNode("-evil"); i.State != "unreachable" || !strings.Contains(i.Error, "invalid node") {
		t.Fatalf("%+v", i)
	}
	if i := m.ProbeNode("ghost"); i.State != "unreachable" || !strings.Contains(i.Error, `unknown node "ghost"`) || strings.Contains(i.Error, "mole node:") {
		t.Fatalf("%+v", i)
	}
}

func TestAddNodeBuildsTheRightCommand(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	stubMole(t, `
[ "$*" = "node help" ] && exit 0
printf '%s\n' "$@" > '`+log+`'`)
	m := NewMoleService()
	if err := m.AddNode(NodeSpec{Name: "devlabs", Target: "u@h:22", SSHPort: 2200, Bin: "~/apps/mole", Insecure: true, Check: false, Replace: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	want := "node\nadd\n-ssh-port\n2200\n-bin\n~/apps/mole\n-insecure\n-no-check\n-force\ndevlabs\nu@h:22\n"
	if string(got) != want {
		t.Fatalf("args:\n%s\nwant:\n%s", got, want)
	}
	if err := m.AddNode(NodeSpec{Name: "b", Target: "h", Check: true}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(log)
	if string(got) != "node\nadd\nb\nh\n" {
		t.Fatalf("a checked add has no -no-check: %q", got)
	}
}

func TestAddNodeValidatesBeforeRunning(t *testing.T) {
	log := filepath.Join(t.TempDir(), "ran")
	stubMole(t, `
[ "$*" = "node help" ] && exit 0
touch '`+log+`'`)
	m := NewMoleService()
	for _, s := range []NodeSpec{
		{Name: "Bad Name", Target: "h"},
		{Name: "", Target: "h"},
		{Name: "ok", Target: ""},
		{Name: "ok", Target: "-oProxyCommand=evil"},
		{Name: "ok", Target: "h", SSHPort: 70000},
	} {
		if err := m.AddNode(s); err == nil {
			t.Errorf("%+v was accepted", s)
		}
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("the CLI ran for an invalid node")
	}
}

func TestAddNodeShowsTheCLIsReason(t *testing.T) {
	stubMole(t, `
[ "$*" = "node help" ] && exit 0
echo 'mole node: cannot reach u@h: dial tcp: i/o timeout' >&2
echo '(use -no-check to register it anyway)' >&2
exit 1`)
	err := NewMoleService().AddNode(NodeSpec{Name: "a", Target: "u@h", Check: true})
	if err == nil || !strings.Contains(err.Error(), "cannot reach u@h") || strings.Contains(err.Error(), "mole node:") {
		t.Fatalf("%v", err)
	}
}

func TestNodeCommandsRunOnTheNode(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	stubMole(t, `
[ "$*" = "node help" ] && exit 0
printf '%s ' "$@" >> '`+log+`'; echo >> '`+log+`'`)
	m := NewMoleService()
	for _, err := range []error{
		m.NodeAddPort("devlabs", 3000), m.NodeRemovePort("devlabs", 3000),
		m.NodeAddPush("devlabs", 3000, 8080), m.NodeAddPush("devlabs", 3000, 3000), m.NodeRemovePush("devlabs", 3000),
		m.NodeStart("devlabs"), m.NodeStop("devlabs"), m.NodeRestart("devlabs"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(log)
	want := "-n devlabs ports add 3000 \n-n devlabs ports remove 3000 \n-n devlabs push 3000:8080 \n-n devlabs push 3000 \n" +
		"-n devlabs unpush 3000 \n-n devlabs up -d \n-n devlabs down \n-n devlabs restart \n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestNodeCommandFailureCarriesTheOutput(t *testing.T) {
	stubMole(t, `
[ "$*" = "node help" ] && exit 0
echo "remote port 80 in use" >&2; exit 1`)
	err := NewMoleService().NodeAddPush("devlabs", 80, 80)
	if err == nil || !strings.Contains(err.Error(), "remote port 80 in use") {
		t.Fatalf("%v", err)
	}
	if _, err := NewMoleService().RunOnNode("--evil", []string{"status"}); err == nil {
		t.Fatal("a node name that is a flag was accepted")
	}
}

func waitJob(t *testing.T, m *MoleService, id uint64) (out string, v NodeJobView) {
	t.Helper()
	from := 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v = m.GetNodeJob(id, from)
		out += v.Output
		from = v.Next
		if v.Done {
			return out, v
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return
}

func TestNodeJobStreamsOutputAndExitCode(t *testing.T) {
	stubMole(t, `echo one; sleep 0.2; echo two >&2; sleep 0.2; echo three; exit 3`)
	m := NewMoleService()
	id, err := m.StartNodeJob("devlabs", []string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	out, v := waitJob(t, m, id)
	if !v.Done || v.Code != 3 || v.Error != "" || out != "one\ntwo\nthree\n" {
		t.Fatalf("out=%q view=%+v", out, v)
	}
	if g := m.GetNodeJob(id+99, 0); g.Found || !g.Done {
		t.Fatalf("unknown job: %+v", g)
	}
}

func TestNodeJobCanBeCancelled(t *testing.T) {
	stubMole(t, `echo started; exec sleep 30`)
	m := NewMoleService()
	id, err := m.StartNodeJob("devlabs", []string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	m.CancelNodeJob(id)
	out, v := waitJob(t, m, id)
	if !v.Done || v.Error != "cancelled" || !strings.Contains(out, "started") {
		t.Fatalf("out=%q view=%+v", out, v)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`ports add 3000`:           {"ports", "add", "3000"},
		`  status   `:              {"status"},
		`send "my file.txt" -to x`: {"send", "my file.txt", "-to", "x"},
		`a 'b c' "d\"e" f\ g`:      {"a", "b c", `d"e`, "f g"},
		`x '' y`:                   {"x", "", "y"},
		``:                         nil,
		`$HOME $(id) ;`:            {"$HOME", "$(id)", ";"},
	}
	for in, want := range cases {
		got, err := splitCommand(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitCommand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`a "b`, `a 'b`, `a\`} {
		if _, err := splitCommand(bad); err == nil {
			t.Errorf("splitCommand(%q) accepted", bad)
		}
	}
}

func TestGetSSHHosts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ssh := filepath.Join(home, ".ssh")
	os.MkdirAll(filepath.Join(ssh, "conf.d"), 0o700)
	os.WriteFile(filepath.Join(ssh, "config"), []byte(`
# comment
Include conf.d/*.conf

Host devlabs dev2
    HostName 10.0.0.5
    User adria
    Port 2222

Host *
    ServerAliveInterval 30

Host !bad web-?
    HostName nope

Match host foo
    User ignored

Host=eq
    HostName eq.example.com
`), 0o600)
	os.WriteFile(filepath.Join(ssh, "conf.d", "x.conf"), []byte("Host included\n  HostName inc.example.com\n"), 0o600)
	got := NewMoleService().GetSSHHosts()
	byAlias := map[string]SSHHost{}
	for _, h := range got {
		byAlias[h.Alias] = h
	}
	if len(got) != 4 {
		t.Fatalf("hosts: %+v", got)
	}
	if d := byAlias["devlabs"]; d.HostName != "10.0.0.5" || d.User != "adria" || d.Port != 2222 || byAlias["dev2"].Port != 2222 {
		t.Fatalf("devlabs: %+v", got)
	}
	if byAlias["included"].HostName != "inc.example.com" || byAlias["eq"].HostName != "eq.example.com" {
		t.Fatalf("%+v", got)
	}
}

func TestNodeInstallJob(t *testing.T) {
	stubMole(t, `printf '%s ' "$@"; echo`)
	m := NewMoleService()
	id, err := m.StartNodeInstall("devlabs", "feat/x")
	if err != nil {
		t.Fatal(err)
	}
	out, v := waitJob(t, m, id)
	if v.Code != 0 || strings.TrimSpace(out) != "node install -branch feat/x devlabs" {
		t.Fatalf("out=%q %+v", out, v)
	}
	id, _ = m.StartNodeInstall("devlabs", "")
	if out, _ := waitJob(t, m, id); strings.TrimSpace(out) != "node install devlabs" {
		t.Fatalf("out=%q", out)
	}
	if _, err := m.StartNodeInstall("devlabs", "--evil"); err == nil {
		t.Fatal("a flag as a branch was accepted")
	}
	if _, err := m.StartNodeInstall("-x", ""); err == nil {
		t.Fatal("a flag as a node was accepted")
	}
}
