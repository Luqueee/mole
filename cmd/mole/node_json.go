package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
)

// nodeInfo is the machine-readable description of a node that
// `mole node ls -json` and `mole node ping -json` print; the desktop app
// renders it.
type nodeInfo struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	SSHPort  int    `json:"sshPort,omitempty"`
	Bin      string `json:"bin,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
	// Implicit is the config's own `remote:`, usable as a node without being registered.
	Implicit bool `json:"implicit,omitempty"`

	// State is "ready" (its agent answered), "no-agent" (mole answers but is
	// too old for the agent), "not-installed" (the node answers, mole is not
	// there) or "unreachable".
	State string `json:"state"`
	Error string `json:"error,omitempty"`

	Version  string `json:"version,omitempty"`
	Build    string `json:"build,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Agent    int    `json:"agent,omitempty"`

	ConnectMs   float64 `json:"connectMs"`
	RoundTripMs float64 `json:"roundTripMs"`

	Daemon *nodeDaemon `json:"daemon,omitempty"`
}

// nodeDaemon is what a node reports about its configuration and its daemon.
type nodeDaemon struct {
	Running     bool             `json:"running"`
	Uptime      string           `json:"uptime,omitempty"`
	ActiveConns int64            `json:"activeConns"`
	TotalConns  uint64           `json:"totalConns"`
	FailedDials uint64           `json:"failedDials"`
	Ports       []int            `json:"ports"` // forwarded right now
	Push        []admin.PushInfo `json:"push"`

	Remote        string `json:"remote,omitempty"`
	AdminAddr     string `json:"adminAddr,omitempty"`
	ConfigPath    string `json:"configPath,omitempty"`
	Inbox         string `json:"inbox,omitempty"`
	Receive       bool   `json:"receive"`
	Accept        string `json:"accept,omitempty"`
	Nodes         int    `json:"nodes"`
	AutoDiscover  bool   `json:"autoDiscover"`
	DiscoverPorts []int  `json:"discoverPorts"`
	ExcludePorts  []int  `json:"excludePorts"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// buildNodeInfo turns the result of probing a node into its JSON form.
func buildNodeInfo(name string, n config.NodeConfig, p probe, implicit bool) nodeInfo {
	info := nodeInfo{
		Name: name, Target: n.Remote, SSHPort: n.SSHPort, Bin: n.Bin, Insecure: n.Insecure, Implicit: implicit,
		ConnectMs: ms(p.Connect), RoundTripMs: ms(p.RoundTrip),
	}
	switch {
	case p.NotInstalled:
		info.State, info.Error = "not-installed", firstLine(p.Err.Error())
		info.ConnectMs, info.RoundTripMs = ms(p.Connect), 0
	case p.Err != nil:
		info.State, info.Error = "unreachable", firstLine(p.Err.Error())
		info.ConnectMs, info.RoundTripMs = 0, 0
	case p.HasAgent:
		info.State = "ready"
		h := p.Hello
		info.Version, info.Build, info.OS, info.Arch, info.Hostname, info.Agent = h.Version, h.Build, h.OS, h.Arch, h.Hostname, h.Agent
		info.Daemon = buildNodeDaemon(p)
	default:
		info.State, info.Version = "no-agent", p.Version
	}
	return info
}

func buildNodeDaemon(p probe) *nodeDaemon {
	s := p.Status
	d := &nodeDaemon{
		Running: s.Running, Remote: s.Remote, AdminAddr: s.AdminAddr, ConfigPath: s.ConfigPath, Inbox: s.Inbox, Nodes: s.Nodes,
		AutoDiscover: s.AutoDiscover, DiscoverPorts: orEmpty(s.DiscoverPorts), ExcludePorts: orEmpty(s.ExcludePorts),
		Ports: []int{}, Push: []admin.PushInfo{},
	}
	if !s.Running {
		return d
	}
	var doc struct {
		Stats struct {
			Uptime      string `json:"uptime"`
			ActiveConns int64  `json:"active_conns"`
			TotalConns  uint64 `json:"total_conns"`
			FailedDials uint64 `json:"failed_dials"`
		} `json:"stats"`
		Info struct {
			Ports    []int            `json:"ports"`
			Push     []admin.PushInfo `json:"push"`
			Transfer struct {
				Receive bool   `json:"receive"`
				Accept  string `json:"accept"`
			} `json:"transfer"`
		} `json:"info"`
	}
	_ = json.Unmarshal(s.Admin, &doc)
	d.Uptime = doc.Stats.Uptime
	d.ActiveConns, d.TotalConns, d.FailedDials = doc.Stats.ActiveConns, doc.Stats.TotalConns, doc.Stats.FailedDials
	d.Ports = orEmpty(doc.Info.Ports)
	if doc.Info.Push != nil {
		d.Push = doc.Info.Push
	}
	d.Receive, d.Accept = doc.Info.Transfer.Receive, doc.Info.Transfer.Accept
	return d
}

func orEmpty(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "mole node:", err)
		return 1
	}
	return 0
}
