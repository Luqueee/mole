// mole node — the other machines this one can manage.
//
//	mole node add NAME TARGET     register a node (TARGET: user@host[:port] or an ssh alias)
//	mole node ls                  every node, whether it answers, and what it runs
//	mole node ping NAME           time a round trip to a node
//	mole node rm NAME             forget a node
//
// Run a command on a node with `mole -n NAME <command>`.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Luqueee/mole/internal/agent"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/tunnel"
)

func printNodeUsage(w *os.File) {
	fmt.Fprintln(w, `Usage: mole node <command>

The other machines this one can manage over SSH. Run any mole command on one
with: mole -n NAME <command>   (e.g. mole -n devlabs ports add 3000)

Commands:
  add NAME TARGET   register a node; TARGET is user@host[:port] or an ssh alias
                    flags: -ssh-port N  -bin PATH  -insecure  -no-check
  ls                every node, whether it answers, its version and daemon
  ping NAME         time a round trip to a node
  rm NAME           forget a node

Flags (every command):
  -config  path to YAML config`)
}

func runNode(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printNodeUsage(os.Stdout)
		return map[bool]int{true: 0, false: 2}[len(args) > 0]
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("node "+cmd, flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config")
	sshPort := fs.Int("ssh-port", 0, "SSH port of the node (add)")
	bin := fs.String("bin", "", "path of mole on the node (add)")
	insecure := fs.Bool("insecure", false, "skip host key verification for the node (add; UNSAFE)")
	noCheck := fs.Bool("no-check", false, "register without contacting the node (add)")
	fs.Usage = func() { printNodeUsage(os.Stderr) }
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	switch cmd {
	case "add":
		if len(rest) != 2 {
			fmt.Fprintln(os.Stderr, "Usage: mole node add NAME TARGET [-ssh-port N] [-bin PATH] [-insecure] [-no-check]")
			return 2
		}
		return nodeAdd(*configPath, rest[0], config.NodeConfig{Remote: rest[1], SSHPort: *sshPort, Bin: *bin, Insecure: *insecure}, *noCheck)
	case "ls", "list":
		return nodeList(*configPath)
	case "ping":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "Usage: mole node ping NAME")
			return 2
		}
		return nodePing(*configPath, rest[0])
	case "rm", "remove":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "Usage: mole node rm NAME")
			return 2
		}
		return nodeRemove(*configPath, rest[0])
	}
	fmt.Fprintf(os.Stderr, "mole node: unknown command %q\n\n", cmd)
	printNodeUsage(os.Stderr)
	return 2
}

// probe is what contacting a node found out.
type probe struct {
	Hello     agent.Hello
	Status    agent.Status
	HasAgent  bool
	Version   string // from `mole version` when the node has no agent
	Err       error  // the node could not be reached
	Connect   time.Duration
	RoundTrip time.Duration
}

// probeNode connects to a node and asks its agent who it is; a node without
// an agent is identified by `mole version` instead.
func probeNode(ctx context.Context, cfg *config.Config, n config.NodeConfig) probe {
	var p probe
	t0 := time.Now()
	mgr, err := dialNode(cfg, n, newLogger("error"))
	if err != nil {
		p.Err = err
		return p
	}
	defer mgr.Close()
	p.Connect = time.Since(t0)

	st, err := mgr.Stream(remoteCommand(n.BinOrDefault(), "agent", "--stdio"))
	if err != nil {
		p.Err = err
		return p
	}
	cl := agent.NewClient(streamConn{st})
	defer cl.Close()
	t1 := time.Now()
	hello, err := cl.Hello(ctx)
	p.RoundTrip = time.Since(t1)
	var notAgent *agent.ErrNotAgent
	switch {
	case err == nil:
		p.HasAgent, p.Hello = true, hello
		if s, err := cl.Status(ctx); err == nil {
			p.Status = s
		}
	case errors.As(err, &notAgent):
		var out syncBuffer
		code, _ := mgr.Exec(ctx, remoteCommand(n.BinOrDefault(), "version"), tunnel.ExecOptions{Stdout: &out, Stderr: &out})
		switch msg := strings.TrimSpace(out.String()); {
		case code == 0:
			p.Version = parseVersionOutput(out.String())
		case code == 127: // the shell's "command not found"
			p.Err = errors.New("mole is not installed there (or not in ~/.local/bin or /usr/local/bin): register the node with -bin PATH")
		case msg != "":
			p.Err = fmt.Errorf("mole is not usable there: %s", firstLine(msg))
		default:
			p.Err = fmt.Errorf("mole exited with status %d on the node", code)
		}
	default:
		p.Err = err
	}
	return p
}

// syncBuffer is a bytes.Buffer safe to use as both stdout and stderr of an
// Exec: the SSH session fills each from its own goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// parseVersionOutput extracts "0.3.0" from `mole v0.3.0` (colour codes stripped).
func parseVersionOutput(s string) string {
	var plain strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b { // skip an ANSI escape sequence
			for i < len(s) && !(s[i] >= 'A' && s[i] <= 'Z' || s[i] >= 'a' && s[i] <= 'z') {
				i++
			}
			continue
		}
		plain.WriteByte(s[i])
	}
	for _, f := range strings.Fields(plain.String()) {
		if v, ok := strings.CutPrefix(f, "v"); ok && v != "" && v[0] >= '0' && v[0] <= '9' {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 160 {
		line = line[:157] + "..."
	}
	return line
}

func nodeAdd(configPath, name string, n config.NodeConfig, noCheck bool) int {
	if err := config.ValidNodeName(name); err != nil {
		fmt.Fprintln(os.Stderr, "mole node:", err)
		return 2
	}
	if err := n.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "mole node:", err)
		return 2
	}
	cfg, path, err := loadTransferForWrite(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if _, exists := cfg.Nodes[name]; exists {
		fmt.Fprintf(os.Stderr, "mole node: %s is already registered (mole node rm %s first)\n", name, name)
		return 1
	}
	color := cliColor(os.Stdout)
	if !noCheck {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		p := probeNode(ctx, cfg, n)
		switch {
		case p.Err != nil:
			fmt.Fprintf(os.Stderr, "mole node: cannot reach %s: %v\n(use -no-check to register it anyway)\n", n.Remote, p.Err)
			return 1
		case p.HasAgent:
			fmt.Printf("%s %s: mole %s on %s (%s/%s)\n", cGreen("✓", color), name, p.Hello.Version, p.Hello.Hostname, p.Hello.OS, p.Hello.Arch)
		default:
			fmt.Printf("%s %s answers, but its mole%s has no agent: update it with `mole -n %s update` once registered\n", cDim("!", color), name, versionSuffix(p.Version), name)
		}
	}
	if cfg.Nodes == nil {
		cfg.Nodes = map[string]config.NodeConfig{}
	}
	cfg.Nodes[name] = n
	if err := config.Save(path, cfg, "nodes"); err != nil {
		fmt.Fprintf(os.Stderr, "mole node: write %s: %v\n", path, err)
		return 1
	}
	fmt.Printf("node %s registered in %s\nrun commands there with: mole -n %s <command>\n", name, path, name)
	return 0
}

func versionSuffix(v string) string {
	if v == "" {
		return ""
	}
	return " (" + v + ")"
}

func nodeRemove(configPath, name string) int {
	cfg, path, err := loadTransferForWrite(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if _, ok := cfg.Nodes[name]; !ok {
		fmt.Fprintf(os.Stderr, "mole node: %s is not registered\n", name)
		return 1
	}
	delete(cfg.Nodes, name)
	if err := config.Save(path, cfg, "nodes"); err != nil {
		fmt.Fprintf(os.Stderr, "mole node: write %s: %v\n", path, err)
		return 1
	}
	fmt.Printf("node %s removed from %s\n", name, path)
	return 0
}

func nodePing(configPath, name string) int {
	cfg, err := config.Load(resolveConfigPathReadOnly(configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	n, err := lookupNode(cfg, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole node:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := probeNode(ctx, cfg, n)
	if p.Err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, p.Err)
		return 1
	}
	if !p.HasAgent {
		fmt.Printf("%s: reachable (connect %s); its mole%s has no agent\n", name, p.Connect.Round(time.Millisecond), versionSuffix(p.Version))
		return 0
	}
	build := ""
	if p.Hello.Build != "" {
		build = ", built from " + p.Hello.Build
	}
	fmt.Printf("%s: connect %s, agent round trip %s (mole %s%s)\n", name, p.Connect.Round(time.Millisecond), p.RoundTrip.Round(time.Millisecond), p.Hello.Version, build)
	return 0
}

// daemonSummary describes the daemon from the agent's status.
func daemonSummary(s agent.Status) string {
	if !s.Running {
		return "down"
	}
	var admin struct {
		Stats struct {
			Uptime string `json:"uptime"`
		} `json:"stats"`
		Info struct {
			Ports []int `json:"ports"`
			Push  []any `json:"push"`
		} `json:"info"`
	}
	_ = json.Unmarshal(s.Admin, &admin)
	out := "up"
	if admin.Stats.Uptime != "" {
		out += " " + admin.Stats.Uptime
	}
	return fmt.Sprintf("%s · %d ports · %d push", out, len(admin.Info.Ports), len(admin.Info.Push))
}

func nodeList(configPath string) int {
	cfg, err := config.Load(resolveConfigPathReadOnly(configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	type row struct {
		name   string
		target string
		node   config.NodeConfig
		p      probe
	}
	var rows []*row
	for _, name := range cfg.NodeNames() {
		rows = append(rows, &row{name: name, target: cfg.Nodes[name].Remote, node: cfg.Nodes[name]})
	}
	if cfg.Remote != "" {
		if _, dup := cfg.Nodes[cfg.Remote]; !dup {
			n, _ := cfg.Node(cfg.Remote)
			rows = append(rows, &row{name: cfg.Remote + " *", target: cfg.Remote, node: n})
		}
	}
	if len(rows) == 0 {
		fmt.Println("no nodes yet: mole node add NAME TARGET")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, r := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.p = probeNode(ctx, cfg, r.node)
		}()
	}
	wg.Wait()

	color := cliColor(os.Stdout)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	fmt.Printf("%-14s %-24s %-12s %-9s %-13s %s\n", "NAME", "TARGET", "STATE", "VERSION", "OS/ARCH", "DAEMON")
	for _, r := range rows {
		state, ver, osarch, daemon := "", "", "", ""
		switch {
		case r.p.Err != nil:
			state = cRed("✗ unreachable", color)
			daemon = cDim(firstLine(r.p.Err.Error()), color)
		case r.p.HasAgent:
			state = cGreen("● ready", color)
			ver = r.p.Hello.Version
			osarch = r.p.Hello.OS + "/" + r.p.Hello.Arch
			daemon = daemonSummary(r.p.Status)
		default:
			state = cDim("○ no agent", color)
			ver = r.p.Version
			daemon = cDim("update it: mole -n "+strings.TrimSuffix(r.name, " *")+" update", color)
		}
		fmt.Printf("%-14s %-24s %-12s %-9s %-13s %s\n", r.name, r.target, padVisible(state, 12), ver, osarch, daemon)
	}
	if cfg.Remote != "" {
		fmt.Println(cDim("\n* the config's own `remote:`, usable as a node without registering it", color))
	}
	return 0
}

// padVisible pads s to width, ignoring ANSI colour codes in its length.
func padVisible(s string, width int) string {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		n++
	}
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}
