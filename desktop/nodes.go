package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
)

// The nodes of the mesh: other machines this one manages over SSH. The app
// asks the mole CLI (`mole node ...`, `mole -n NODE ...`) for everything, as
// it does for the rest, so the CLI stays the single source of truth.

// NodeEntry is a node as the config knows it. Nothing here needs the network.
type NodeEntry struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	SSHPort  int    `json:"sshPort"`
	Bin      string `json:"bin"`
	Insecure bool   `json:"insecure"`
	// Implicit is the config's own `remote:`: usable as a node without being registered.
	Implicit bool `json:"implicit"`
}

// SelfInfo describes this machine, the hub of the map.
type SelfInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// NodeList is what the Nodes tab starts from.
type NodeList struct {
	Self  SelfInfo    `json:"self"`
	Nodes []NodeEntry `json:"nodes"`
	// CLIReady is false when the installed mole CLI predates nodes.
	CLIReady bool `json:"cliReady"`
	// CLIError says why the CLI could not be asked (not installed, too old).
	CLIError string `json:"cliError,omitempty"`
}

// NodeDaemon is what a node reports about its configuration and its daemon.
type NodeDaemon struct {
	Running       bool             `json:"running"`
	Uptime        string           `json:"uptime"`
	ActiveConns   int64            `json:"activeConns"`
	TotalConns    uint64           `json:"totalConns"`
	FailedDials   uint64           `json:"failedDials"`
	Ports         []int            `json:"ports"`
	Push          []admin.PushInfo `json:"push"`
	Remote        string           `json:"remote"`
	AdminAddr     string           `json:"adminAddr"`
	ConfigPath    string           `json:"configPath"`
	Inbox         string           `json:"inbox"`
	Receive       bool             `json:"receive"`
	Accept        string           `json:"accept"`
	Nodes         int              `json:"nodes"`
	AutoDiscover  bool             `json:"autoDiscover"`
	DiscoverPorts []int            `json:"discoverPorts"`
	ExcludePorts  []int            `json:"excludePorts"`
}

// NodeInfo is the result of contacting one node.
type NodeInfo struct {
	Name        string      `json:"name"`
	Target      string      `json:"target"`
	SSHPort     int         `json:"sshPort"`
	Bin         string      `json:"bin"`
	Insecure    bool        `json:"insecure"`
	Implicit    bool        `json:"implicit"`
	State       string      `json:"state"` // ready | no-agent | unreachable
	Error       string      `json:"error"`
	Version     string      `json:"version"`
	Build       string      `json:"build"`
	OS          string      `json:"os"`
	Arch        string      `json:"arch"`
	Hostname    string      `json:"hostname"`
	Agent       int         `json:"agent"`
	ConnectMs   float64     `json:"connectMs"`
	RoundTripMs float64     `json:"roundTripMs"`
	Daemon      *NodeDaemon `json:"daemon"`
	// CLIOutdated: the local CLI could not even be asked (see NodeList.CLIError).
	CLIOutdated bool `json:"cliOutdated"`
}

// NodeSpec is a node to register.
type NodeSpec struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	SSHPort  int    `json:"sshPort"`
	Bin      string `json:"bin"`
	Insecure bool   `json:"insecure"`
	// Check contacts the node first and refuses one that does not answer.
	Check bool `json:"check"`
	// Replace overwrites a node already registered under that name.
	Replace bool `json:"replace"`
}

// cliRun is the outcome of one run of the mole CLI.
type cliRun struct {
	Stdout string
	Stderr string
	Code   int
}

// runMole runs the mole CLI and waits for it. A command that ends with a
// non-zero status is not an error here: the caller reads Code.
func runMole(ctx context.Context, args ...string) (cliRun, error) {
	bin, err := moleBin()
	if err != nil {
		return cliRun{}, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	err = cmd.Run()
	r := cliRun{Stdout: stdout.String(), Stderr: stderr.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.Code = ee.ExitCode()
	default:
		return r, err
	}
	if ctx.Err() != nil {
		return r, fmt.Errorf("timed out: %w", ctx.Err())
	}
	return r, nil
}

// cliSupportsNodes reports whether the installed CLI has `mole node`. Only a
// positive answer is remembered: the user may update the CLI at any moment.
var nodesSupported struct {
	mu sync.Mutex
	ok bool
}

func (m *MoleService) cliSupportsNodes() (bool, string) {
	nodesSupported.mu.Lock()
	defer nodesSupported.mu.Unlock()
	if nodesSupported.ok {
		return true, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := runMole(ctx, "node", "help")
	switch {
	case err != nil:
		return false, err.Error()
	case r.Code != 0:
		return false, "the installed mole CLI predates nodes: update it from SETUP"
	}
	nodesSupported.ok = true
	return true, ""
}

// GetNodes lists the registered nodes from the config, without contacting any.
func (m *MoleService) GetNodes() NodeList {
	host, _ := os.Hostname()
	out := NodeList{Self: SelfInfo{Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH}, Nodes: []NodeEntry{}}
	out.CLIReady, out.CLIError = m.cliSupportsNodes()
	cfg, _, err := loadConfig()
	if err != nil {
		return out
	}
	for _, name := range cfg.NodeNames() {
		n := cfg.Nodes[name]
		out.Nodes = append(out.Nodes, NodeEntry{Name: name, Target: n.Remote, SSHPort: n.SSHPort, Bin: n.Bin, Insecure: n.Insecure})
	}
	if cfg.Remote != "" && !cfg.IsRegistered(cfg.Remote) {
		out.Nodes = append(out.Nodes, NodeEntry{Name: cfg.Remote, Target: cfg.Remote, SSHPort: cfg.SSHPort, Insecure: cfg.Insecure, Implicit: true})
	}
	return out
}

// ProbeNode contacts one node (connection, agent hello and status). An
// unreachable node is a result, not an error.
func (m *MoleService) ProbeNode(name string) NodeInfo {
	fail := func(msg string, outdated bool) NodeInfo {
		return NodeInfo{Name: name, State: "unreachable", Error: msg, CLIOutdated: outdated}
	}
	if ok, why := m.cliSupportsNodes(); !ok {
		return fail(why, true)
	}
	if err := checkNodeName(name); err != nil {
		return fail(err.Error(), false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := runMole(ctx, "node", "ping", "-json", name)
	if err != nil {
		return fail(err.Error(), false)
	}
	if r.Code != 0 {
		return fail(cleanCLIError(r.Stderr+r.Stdout), false)
	}
	var info NodeInfo
	if err := json.Unmarshal([]byte(r.Stdout), &info); err != nil {
		return fail("unreadable answer from the mole CLI: "+err.Error(), false)
	}
	info.Name = name
	if info.Daemon != nil {
		if info.Daemon.Ports == nil {
			info.Daemon.Ports = []int{}
		}
		if info.Daemon.Push == nil {
			info.Daemon.Push = []admin.PushInfo{}
		}
		if info.Daemon.DiscoverPorts == nil {
			info.Daemon.DiscoverPorts = []int{}
		}
		if info.Daemon.ExcludePorts == nil {
			info.Daemon.ExcludePorts = []int{}
		}
	}
	return info
}

// checkNodeName refuses what could not be a node name or target, and in
// particular anything that would be read as a flag.
func checkNodeName(name string) error {
	if strings.TrimSpace(name) == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "\x00\n\r") {
		return fmt.Errorf("invalid node %q", name)
	}
	return nil
}

// cleanCLIError makes the CLI's complaint fit a toast: no prefix, no usage banner.
func cleanCLIError(s string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "mole node:"))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Usage:") {
			break
		}
		keep = append(keep, line)
	}
	if len(keep) == 0 {
		return "the mole CLI failed"
	}
	return strings.Join(keep, "\n")
}

// AddNode registers a node.
func (m *MoleService) AddNode(spec NodeSpec) error {
	spec.Name, spec.Target = strings.TrimSpace(spec.Name), strings.TrimSpace(spec.Target)
	if err := config.ValidNodeName(spec.Name); err != nil {
		return err
	}
	if err := (config.NodeConfig{Remote: spec.Target, SSHPort: spec.SSHPort, Bin: spec.Bin}).Validate(); err != nil {
		return err
	}
	if err := checkNodeName(spec.Target); err != nil {
		return fmt.Errorf("invalid target %q", spec.Target)
	}
	if ok, why := m.cliSupportsNodes(); !ok {
		return errors.New(why)
	}
	args := []string{"node", "add"}
	if spec.SSHPort != 0 {
		args = append(args, "-ssh-port", strconv.Itoa(spec.SSHPort))
	}
	if b := strings.TrimSpace(spec.Bin); b != "" {
		args = append(args, "-bin", b)
	}
	if spec.Insecure {
		args = append(args, "-insecure")
	}
	if !spec.Check {
		args = append(args, "-no-check")
	}
	if spec.Replace {
		args = append(args, "-force")
	}
	args = append(args, spec.Name, spec.Target)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	r, err := runMole(ctx, args...)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		return errors.New(cleanCLIError(r.Stderr + r.Stdout))
	}
	return nil
}

// RemoveNode forgets a registered node.
func (m *MoleService) RemoveNode(name string) error {
	if err := checkNodeName(name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := runMole(ctx, "node", "rm", name)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		return errors.New(cleanCLIError(r.Stderr + r.Stdout))
	}
	return nil
}

// NodeRunResult is the outcome of a command run on a node.
type NodeRunResult struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
}

// RunOnNode runs `mole <args>` on a node and waits for it (for quick
// commands: adding a port, starting its daemon). Long ones use StartNodeJob.
func (m *MoleService) RunOnNode(name string, args []string) (NodeRunResult, error) {
	if err := checkNodeName(name); err != nil {
		return NodeRunResult{}, err
	}
	if len(args) == 0 {
		return NodeRunResult{}, errors.New("no command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r, err := runMole(ctx, append([]string{"-n", name}, args...)...)
	if err != nil {
		return NodeRunResult{}, err
	}
	return NodeRunResult{Output: strings.TrimSpace(r.Stdout + r.Stderr), Code: r.Code}, nil
}

// nodeOp runs a command on a node and turns a failure into an error.
func (m *MoleService) nodeOp(name string, args ...string) error {
	r, err := m.RunOnNode(name, args)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		msg := r.Output
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", r.Code)
		}
		return errors.New(msg)
	}
	return nil
}

func (m *MoleService) NodeAddPort(name string, port int) error {
	return m.nodeOp(name, "ports", "add", strconv.Itoa(port))
}
func (m *MoleService) NodeRemovePort(name string, port int) error {
	return m.nodeOp(name, "ports", "remove", strconv.Itoa(port))
}
func (m *MoleService) NodeAddPush(name string, local, remote int) error {
	spec := strconv.Itoa(local)
	if remote != 0 && remote != local {
		spec += ":" + strconv.Itoa(remote)
	}
	return m.nodeOp(name, "push", spec)
}
func (m *MoleService) NodeRemovePush(name string, local int) error {
	return m.nodeOp(name, "unpush", strconv.Itoa(local))
}
func (m *MoleService) NodeStart(name string) error   { return m.nodeOp(name, "up", "-d") }
func (m *MoleService) NodeStop(name string) error    { return m.nodeOp(name, "down") }
func (m *MoleService) NodeRestart(name string) error { return m.nodeOp(name, "restart") }

// ─── jobs: commands that take a while, with their output as it arrives ───

// NodeJobView is a job's progress. Output holds what was written after the
// byte offset the caller asked from; Next is where to continue.
type NodeJobView struct {
	Found  bool   `json:"found"`
	Output string `json:"output"`
	Next   int    `json:"next"`
	Done   bool   `json:"done"`
	Code   int    `json:"code"`
	Error  string `json:"error"`
}

type nodeJob struct {
	mu     sync.Mutex
	out    []byte
	done   bool
	code   int
	err    string
	cancel context.CancelFunc
	ended  time.Time
}

func (j *nodeJob) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.out) < maxJobOutput {
		j.out = append(j.out, p...)
	}
	return len(p), nil
}

const (
	maxJobOutput = 1 << 20
	maxJobs      = 24
	jobTimeout   = 15 * time.Minute
)

var jobs = struct {
	mu   sync.Mutex
	next uint64
	all  map[uint64]*nodeJob
}{all: map[uint64]*nodeJob{}}

// StartNodeJob runs `mole -n NAME <args>` in the background and returns an
// id for GetNodeJob. It has no terminal: a command that asks for one fails.
func (m *MoleService) StartNodeJob(name string, args []string) (uint64, error) {
	if err := checkNodeName(name); err != nil {
		return 0, err
	}
	if len(args) == 0 {
		return 0, errors.New("no command")
	}
	bin, err := moleBin()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	job := &nodeJob{cancel: cancel}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-n", name}, args...)...)
	cmd.Stdout, cmd.Stderr = job, job
	cmd.Env = append(os.Environ(), "NO_COLOR=1")

	jobs.mu.Lock()
	jobs.next++
	id := jobs.next
	jobs.all[id] = job
	pruneJobs()
	jobs.mu.Unlock()

	go func() {
		defer cancel()
		err := cmd.Run()
		job.mu.Lock()
		defer job.mu.Unlock()
		job.done, job.ended = true, time.Now()
		var ee *exec.ExitError
		switch {
		case err == nil:
		case ctx.Err() == context.DeadlineExceeded:
			job.code, job.err = -1, "timed out"
		case ctx.Err() != nil:
			job.code, job.err = -1, "cancelled"
		case errors.As(err, &ee):
			job.code = ee.ExitCode()
		default:
			job.code, job.err = -1, err.Error()
		}
	}()
	return id, nil
}

// pruneJobs forgets the oldest finished jobs. jobs.mu must be held.
func pruneJobs() {
	for len(jobs.all) > maxJobs {
		var oldest uint64
		var at time.Time
		for id, j := range jobs.all {
			j.mu.Lock()
			done, ended := j.done, j.ended
			j.mu.Unlock()
			if done && (oldest == 0 || ended.Before(at)) {
				oldest, at = id, ended
			}
		}
		if oldest == 0 {
			return
		}
		delete(jobs.all, oldest)
	}
}

// GetNodeJob returns a job's output from the byte offset from.
func (m *MoleService) GetNodeJob(id uint64, from int) NodeJobView {
	jobs.mu.Lock()
	job := jobs.all[id]
	jobs.mu.Unlock()
	if job == nil {
		return NodeJobView{Done: true}
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	from = max(0, min(from, len(job.out)))
	return NodeJobView{Found: true, Output: strings.ToValidUTF8(string(job.out[from:]), "�"), Next: len(job.out), Done: job.done, Code: job.code, Error: job.err}
}

// CancelNodeJob stops a running job.
func (m *MoleService) CancelNodeJob(id uint64) {
	jobs.mu.Lock()
	job := jobs.all[id]
	jobs.mu.Unlock()
	if job != nil {
		job.cancel()
	}
}

// SplitCommand splits a typed command line into arguments: words separated
// by spaces, with '...' and "..." grouping and backslash escaping one
// character. No variable or glob expansion happens; the words go to mole as is.
func (m *MoleService) SplitCommand(line string) ([]string, error) {
	return splitCommand(line)
}

func splitCommand(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	var quote rune
	esc := false
	for _, r := range line {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				esc = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			esc, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if esc {
		return nil, errors.New("a backslash at the end of the line")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed %c", quote)
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// ─── suggestions: the hosts of ~/.ssh/config ───

// SSHHost is a host alias from the user's ssh config.
type SSHHost struct {
	Alias    string `json:"alias"`
	HostName string `json:"hostName"`
	User     string `json:"user"`
	Port     int    `json:"port"`
}

// GetSSHHosts lists the concrete hosts of ~/.ssh/config (wildcard patterns
// are skipped), to suggest as nodes.
func (m *MoleService) GetSSHHosts() []SSHHost {
	home, err := os.UserHomeDir()
	if err != nil {
		return []SSHHost{}
	}
	hosts := parseSSHConfig(filepath.Join(home, ".ssh", "config"), filepath.Join(home, ".ssh"), 0)
	sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].Alias < hosts[j].Alias })
	if hosts == nil {
		return []SSHHost{}
	}
	return hosts
}

func parseSSHConfig(path, base string, depth int) []SSHHost {
	f, err := os.Open(path)
	if err != nil || depth > 3 {
		return nil
	}
	defer f.Close()
	var out []SSHHost
	var cur []int // indexes in out of the aliases the current Host block applies to
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			key, val, ok = strings.Cut(line, "=")
		}
		if !ok {
			continue
		}
		key, val = strings.ToLower(strings.TrimSpace(key)), strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(val), "=")), `"`)
		switch key {
		case "host":
			cur = cur[:0]
			for _, a := range strings.Fields(val) {
				if strings.ContainsAny(a, "*?!") {
					continue
				}
				out = append(out, SSHHost{Alias: a})
				cur = append(cur, len(out)-1)
			}
		case "match":
			cur = cur[:0]
		case "hostname":
			for _, i := range cur {
				out[i].HostName = val
			}
		case "user":
			for _, i := range cur {
				out[i].User = val
			}
		case "port":
			p, _ := strconv.Atoi(val)
			for _, i := range cur {
				out[i].Port = p
			}
		case "include":
			for _, pat := range strings.Fields(val) {
				if !filepath.IsAbs(pat) {
					pat = filepath.Join(base, pat)
				}
				files, _ := filepath.Glob(pat)
				for _, file := range files {
					out = append(out, parseSSHConfig(file, base, depth+1)...)
				}
			}
		}
	}
	return out
}
