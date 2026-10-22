package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Luqueee/mole/internal/config"
)

// MoleService exposes the mole daemon to the frontend. Status and port
// changes go through the daemon's admin API; lifecycle commands shell out
// to the `mole` binary so the CLI stays the single source of truth.
type MoleService struct {
	client *http.Client
}

func NewMoleService() *MoleService {
	return &MoleService{client: &http.Client{Timeout: 3 * time.Second}}
}

// Stats mirrors the "stats" object of the admin /status response.
type Stats struct {
	Uptime      string `json:"uptime"`
	ActiveConns int64  `json:"active_conns"`
	TotalConns  uint64 `json:"total_conns"`
	FailedDials uint64 `json:"failed_dials"`
}

// Status is what the frontend renders on its dashboard.
type Status struct {
	Running   bool           `json:"running"`
	AdminAddr string         `json:"adminAddr"`
	Remote    string         `json:"remote"`
	Stats     Stats          `json:"stats"`
	Ports     []int          `json:"ports"`
	Info      map[string]any `json:"info"`
	Error     string         `json:"error,omitempty"`
}

// Settings is the editable subset of the mole config.
type Settings struct {
	Path          string `json:"path"`
	Remote        string `json:"remote"`
	AutoDiscover  bool   `json:"autoDiscover"`
	DiscoverPorts []int  `json:"discoverPorts"`
	Ports         []int  `json:"ports"`
	ExcludePorts  []int  `json:"excludePorts"`
	AdminAddr     string `json:"adminAddr"`
}

func loadConfig() (*config.Config, string, error) {
	path := config.Find()
	cfg, err := config.Load(path)
	return cfg, path, err
}

// GetStatus queries the daemon. A daemon that is down is not an error: it
// returns Running=false so the UI can offer to start it.
func (m *MoleService) GetStatus() Status {
	cfg, _, err := loadConfig()
	if err != nil {
		return Status{Error: err.Error()}
	}
	st := Status{AdminAddr: cfg.AdminAddr, Remote: cfg.Remote}
	if cfg.AdminAddr == "" {
		st.Error = "admin_addr is not set in the mole config; status is unavailable"
		return st
	}
	resp, err := m.client.Get("http://" + cfg.AdminAddr + "/status")
	if err != nil {
		return st
	}
	defer resp.Body.Close()
	var body struct {
		Stats Stats          `json:"stats"`
		Info  map[string]any `json:"info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		st.Error = "bad /status response: " + err.Error()
		return st
	}
	st.Running, st.Stats, st.Info = true, body.Stats, body.Info
	defer func() {
		// Daemons that predate live port reporting answer with an empty
		// list even while forwarding; ask the OS what the process holds.
		if len(st.Ports) == 0 {
			st.Ports = listeningPorts(cfg.AdminAddr)
		}
	}()
	if raw, ok := body.Info["ports"].([]any); ok {
		for _, p := range raw {
			if f, ok := p.(float64); ok {
				st.Ports = append(st.Ports, int(f))
			}
		}
	}
	return st
}

// Event is one finished forwarded connection (mirror of admin.Event).
type Event struct {
	ID       uint64 `json:"id"`
	Port     int    `json:"port"`
	StartMs  int64  `json:"startMs"`
	DurMs    int64  `json:"durMs"`
	BytesOut int64  `json:"bytesOut"`
	BytesIn  int64  `json:"bytesIn"`
	Failed   bool   `json:"failed"`
}

// Events is the answer of GetEvents. Supported is false for daemons that
// predate the /events endpoint.
type Events struct {
	Supported bool    `json:"supported"`
	Last      uint64  `json:"last"`
	Events    []Event `json:"events"`
}

// GetEvents returns the connections finished after event `since`.
func (m *MoleService) GetEvents(since uint64) Events {
	none := Events{Events: []Event{}}
	cfg, _, err := loadConfig()
	if err != nil || cfg.AdminAddr == "" {
		return none
	}
	resp, err := m.client.Get("http://" + cfg.AdminAddr + "/events?since=" + strconv.FormatUint(since, 10))
	if err != nil {
		return none
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return none
	}
	var body struct {
		Last   uint64 `json:"last"`
		Events []struct {
			ID       uint64 `json:"id"`
			Port     int    `json:"port"`
			StartMs  int64  `json:"start_ms"`
			DurMs    int64  `json:"dur_ms"`
			BytesOut int64  `json:"bytes_out"`
			BytesIn  int64  `json:"bytes_in"`
			Failed   bool   `json:"failed"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return none
	}
	out := Events{Supported: true, Last: body.Last, Events: make([]Event, 0, len(body.Events))}
	for _, e := range body.Events {
		out.Events = append(out.Events, Event{e.ID, e.Port, e.StartMs, e.DurMs, e.BytesOut, e.BytesIn, e.Failed})
	}
	return out
}

// listeningPorts lists the loopback TCP ports the mole daemon listens on,
// minus its own admin port. Unix only (lsof); elsewhere it returns nil.
func listeningPorts(adminAddr string) []int {
	data, err := os.ReadFile(filepath.Join(config.StateDir(), "mole.pid"))
	if err != nil {
		return nil
	}
	pid := strings.TrimSpace(string(data))
	out, err := exec.Command("lsof", "-nP", "-a", "-p", pid, "-iTCP", "-sTCP:LISTEN", "-Fn").Output()
	if err != nil {
		return nil
	}
	_, adminPort, _ := net.SplitHostPort(adminAddr)
	var ports []int
	for _, line := range strings.Split(string(out), "\n") {
		name, ok := strings.CutPrefix(line, "n127.0.0.1:")
		if !ok || name == adminPort {
			continue
		}
		if p, err := strconv.Atoi(name); err == nil {
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	return ports
}

// GetSettings returns the active config.
func (m *MoleService) GetSettings() (Settings, error) {
	cfg, path, err := loadConfig()
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		Path: path, Remote: cfg.Remote, AutoDiscover: cfg.AutoDiscover,
		DiscoverPorts: cfg.DiscoverPorts, Ports: cfg.Ports,
		ExcludePorts: cfg.ExcludePorts, AdminAddr: cfg.AdminAddr,
	}, nil
}

// AddDiscoverPort adds a discovery candidate, live when the daemon is up.
func (m *MoleService) AddDiscoverPort(port int) error {
	return m.mole("ports", "add", fmt.Sprint(port))
}

// RemoveDiscoverPort removes a discovery candidate.
func (m *MoleService) RemoveDiscoverPort(port int) error {
	return m.mole("ports", "remove", fmt.Sprint(port))
}

func (m *MoleService) Start() error   { return m.mole("up", "-d") }
func (m *MoleService) Stop() error    { return m.mole("down") }
func (m *MoleService) Restart() error { return m.mole("restart") }

// GetLogs returns the last n lines of the daemon log. Only the tail of the
// file is read: the log grows without bound.
func (m *MoleService) GetLogs(n int) ([]string, error) {
	f, err := os.Open(filepath.Join(config.StateDir(), "mole.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	const tail = 512 << 10
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := max(st.Size()-tail, 0)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if off > 0 {
		lines = lines[1:] // first line is likely cut mid-way
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func (m *MoleService) mole(args ...string) error {
	bin, err := exec.LookPath("mole")
	if err != nil {
		return fmt.Errorf("mole binary not found in PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mole %s: %s", strings.Join(args, " "), strings.TrimSpace(out.String()))
	}
	return nil
}
