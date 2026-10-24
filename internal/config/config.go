// Package config defines the runtime configuration for mole
// and helpers to load it from YAML and merge CLI overrides.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Luqueee/mole/internal/meshdns"
)

// DefaultClipListen is the least-exposure bind address for the unauthenticated
// clipboard endpoint. Use an explicit private interface address for remote
// access over Tailscale or WireGuard.
const DefaultClipListen = "127.0.0.1:7777"

// Config holds the runtime configuration for mole.
type Config struct {
	// Remote is the SSH target in the form user@host[:port].
	// Example: "dev@workstation" or "dev@192.168.1.10:2222".
	Remote string `yaml:"remote"`

	// Ports is the explicit list of local ports to forward.
	// On the remote, the same port number is used.
	Ports []int `yaml:"ports"`

	// Push lists local services exposed on the remote (ssh -R), over the
	// same SSH connection as Ports. Always explicit; never auto-discovered.
	Push []PushRule `yaml:"push"`

	// Domains are DNS suffixes (e.g. "kena.lan") whose names are resolved
	// on the remote: a local DNS stub hands out virtual IPs and mole
	// forwards connections to those IPs by name. See `mole dns install`.
	Domains []string `yaml:"domains"`

	// DomainPorts are the ports forwarded for every name under Domains.
	// Default: 80 and 443.
	DomainPorts []int `yaml:"domain_ports"`

	// AutoDiscover, if true, probes the remote for common dev ports
	// (from DiscoverPorts) and forwards the ones that respond.
	AutoDiscover bool `yaml:"auto_discover"`

	// DiscoverPorts is the list of port numbers to probe when
	// AutoDiscover is enabled. Overrides the built-in defaults.
	DiscoverPorts []int `yaml:"discover_ports"`

	// ExcludePorts are never auto-forwarded (system/reserved ports like
	// SSH, SMTP, DNS). Auto-discovery skips them. Setting this in the
	// config replaces the built-in default; an empty list excludes
	// nothing. Explicit Ports are always forwarded regardless.
	ExcludePorts []int `yaml:"exclude_ports"`

	// AdminAddr is the address of the local admin HTTP API.
	// Set to empty string to disable.
	AdminAddr string `yaml:"admin_addr"`

	// LogLevel controls verbosity: "debug", "info", "warn", "error".
	LogLevel string `yaml:"log_level"`

	// SSHPort is the port on the remote to connect to for SSH (default 22).
	SSHPort int `yaml:"ssh_port"`

	// Insecure disables SSH host key verification (legacy
	// InsecureIgnoreHostKey behaviour). Off by default; only enable on
	// trusted networks for throwaway hosts. Equivalent to `--insecure`.
	Insecure bool `yaml:"insecure"`

	// ClipURL is the HTTP endpoint of the clip server on the Mac
	// (e.g. http://100.64.0.10:7777), as reachable from the LXC over
	// Tailscale or WireGuard. Read by `mole clip pull`.
	ClipURL string `yaml:"clip_url"`

	// ClipListen is the address the clip server binds on the Mac.
	// It defaults to loopback because the endpoint has no authentication.
	// Set it to an explicit private Tailscale or WireGuard address for
	// remote access. Read by `mole clip serve`.
	ClipListen string `yaml:"clip_listen"`

	// ClipIntervalMs controls the clipboard poll cadence on the Mac.
	// 0 falls back to the runClipServe flag default (500ms).
	ClipIntervalMs int `yaml:"clip_interval_ms"`

	// Transfer configures `mole send` and this machine's inbox.
	Transfer TransferConfig `yaml:"transfer"`

	// Nodes are the other machines this one can reach and manage, by name:
	// `mole -n NAME <command>` runs a command on one. Empty by default; the
	// `remote:` above stays the default peer for everything that has no -n.
	Nodes map[string]NodeConfig `yaml:"nodes"`
}

// NodeConfig describes how to reach another mole node over SSH.
type NodeConfig struct {
	// Remote is the SSH target: user@host[:port] or an ssh config alias.
	Remote string `yaml:"remote"`
	// SSHPort overrides the SSH port (default: the config's ssh_port).
	SSHPort int `yaml:"ssh_port,omitempty"`
	// Insecure disables host key verification for this node (UNSAFE).
	Insecure bool `yaml:"insecure,omitempty"`
	// Bin is the mole executable on the node (default "mole", looked up in
	// ~/.local/bin and the usual places).
	Bin string `yaml:"bin,omitempty"`
}

// MaxNodeName bounds a node name.
const MaxNodeName = 32

// ValidNodeName reports whether name is usable as a node name: lowercase
// letters, digits, '-' and '_', starting with a letter or digit.
func ValidNodeName(name string) error {
	if name == "" || len(name) > MaxNodeName {
		return fmt.Errorf("node name %q must be 1-%d characters", name, MaxNodeName)
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || (i > 0 && (r == '-' || r == '_'))
		if !ok {
			return fmt.Errorf("node name %q: use lowercase letters, digits, '-' and '_', starting with a letter or digit", name)
		}
	}
	return nil
}

// Validate checks a node.
func (n NodeConfig) Validate() error {
	if strings.TrimSpace(n.Remote) == "" {
		return errors.New("remote is required")
	}
	if n.SSHPort < 0 || n.SSHPort > 65535 {
		return fmt.Errorf("ssh_port %d out of range", n.SSHPort)
	}
	if strings.ContainsAny(n.Bin, "\x00\n") {
		return errors.New("bin must not contain control characters")
	}
	return nil
}

// BinOrDefault returns the mole executable name or path on the node.
func (n NodeConfig) BinOrDefault() string {
	if n.Bin == "" {
		return "mole"
	}
	return n.Bin
}

// Node resolves a node by name: a registered one, or the config's own
// `remote:` target (so `mole -n <remote>` keeps working without registering
// it). The second result is false if there is no such node.
func (c *Config) Node(name string) (NodeConfig, bool) {
	if n, ok := c.Nodes[name]; ok {
		return n, true
	}
	if c.Remote != "" && name == c.Remote {
		return NodeConfig{Remote: c.Remote, SSHPort: c.SSHPort, Insecure: c.Insecure}, true
	}
	return NodeConfig{}, false
}

// IsRegistered reports whether name is in the node registry (as opposed to
// being the config's own `remote:`).
func (c *Config) IsRegistered(name string) bool {
	_, ok := c.Nodes[name]
	return ok
}

// NodeNames returns the registered node names, sorted.
func (c *Config) NodeNames() []string {
	names := make([]string, 0, len(c.Nodes))
	for n := range c.Nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// DefaultTransferPort is the loopback port a receiving daemon listens on,
// and the port the push forward exposes on the remote, for remote-to-local
// transfers.
const DefaultTransferPort = 9977

// TransferConfig configures file transfer between the two machines.
type TransferConfig struct {
	// Inbox is where files sent to THIS machine land. Absolute, or starting
	// with ~/ (default ~/mole-inbox).
	Inbox string `yaml:"inbox,omitempty"`
	// Receive lets the daemon accept files sent from the remote. Off by
	// default: it exposes a port on the remote that writes into this
	// machine. Sending from this machine to the remote needs no setting.
	Receive bool `yaml:"receive,omitempty"`
	// Accept is "ask" (default: each incoming transfer waits for the user)
	// or "auto" (incoming transfers are saved straight away).
	Accept string `yaml:"accept,omitempty"`
	// Port is the loopback port used for remote-to-local transfers
	// (default DefaultTransferPort).
	Port int `yaml:"port,omitempty"`
	// MaxSizeMB caps one incoming transfer, in MiB (0 = no cap).
	MaxSizeMB int `yaml:"max_size_mb,omitempty"`
}

// InboxDir returns the absolute inbox folder, expanding a leading ~.
func (t TransferConfig) InboxDir() string {
	home, _ := os.UserHomeDir()
	d := t.Inbox
	switch {
	case d == "":
		d = filepath.Join(home, "mole-inbox")
	case d == "~" || strings.HasPrefix(d, "~/") || strings.HasPrefix(d, `~\`):
		d = filepath.Join(home, d[1:])
	}
	return filepath.Clean(d)
}

// AcceptMode returns "ask" or "auto".
func (t TransferConfig) AcceptMode() string {
	if t.Accept == "auto" {
		return "auto"
	}
	return "ask"
}

// PortOrDefault returns the transfer port.
func (t TransferConfig) PortOrDefault() int {
	if t.Port == 0 {
		return DefaultTransferPort
	}
	return t.Port
}

// MaxBytes returns the incoming-transfer cap in bytes (0 = none).
func (t TransferConfig) MaxBytes() int64 { return int64(t.MaxSizeMB) << 20 }

// Validate checks the transfer settings.
func (t TransferConfig) Validate() error {
	if t.Inbox != "" && !filepath.IsAbs(t.Inbox) && t.Inbox != "~" && !strings.HasPrefix(t.Inbox, "~/") && !strings.HasPrefix(t.Inbox, `~\`) {
		return fmt.Errorf("transfer.inbox: %q must be an absolute path or start with ~/", t.Inbox)
	}
	if t.Accept != "" && t.Accept != "ask" && t.Accept != "auto" {
		return fmt.Errorf("transfer.accept: %q must be \"ask\" or \"auto\"", t.Accept)
	}
	if t.Port != 0 && (t.Port < 1 || t.Port > 65535) {
		return fmt.Errorf("transfer.port: %d out of range (1-65535)", t.Port)
	}
	if t.MaxSizeMB < 0 {
		return fmt.Errorf("transfer.max_size_mb: %d must not be negative", t.MaxSizeMB)
	}
	return nil
}

// DefaultPushBind is the remote address push forwards bind to unless the
// rule says otherwise. Loopback keeps the port reachable only from the
// remote host itself; exposing it further needs GatewayPorts on sshd.
const DefaultPushBind = "127.0.0.1"

// PushRule exposes the local port Local on the remote at Remote.
type PushRule struct {
	Local  int    `yaml:"local"`
	Remote int    `yaml:"remote,omitempty"` // default: same as Local
	Bind   string `yaml:"bind,omitempty"`   // default: DefaultPushBind
}

// RemotePort returns the port opened on the remote.
func (r PushRule) RemotePort() int {
	if r.Remote == 0 {
		return r.Local
	}
	return r.Remote
}

// BindAddr returns the remote bind address.
func (r PushRule) BindAddr() string {
	if r.Bind == "" {
		return DefaultPushBind
	}
	return r.Bind
}

// Validate checks port ranges.
func (r PushRule) Validate() error {
	if r.Local < 1 || r.Local > 65535 {
		return fmt.Errorf("push: local port %d out of range (1-65535)", r.Local)
	}
	if p := r.RemotePort(); p < 1 || p > 65535 {
		return fmt.Errorf("push: remote port %d out of range (1-65535)", p)
	}
	return nil
}

// Default returns a Config populated with sensible defaults.
func Default() *Config {
	return &Config{
		AdminAddr: "",
		DiscoverPorts: []int{
			3000, 3001, 3002, 3003, 3004, 3005,
			4200, 5173, 5174, 5327,
			6006, 8000, 8080, 8081, 8443, 9000, 9090,
		},
		// System/reserved ports that are almost never a dev server.
		// Override via `exclude_ports:` in the config.
		ExcludePorts: []int{
			22,  // SSH (the transport itself)
			25,  // SMTP
			53,  // DNS
			111, // rpcbind
			631, // CUPS / printing
		},
		LogLevel:   "info",
		SSHPort:    22,
		ClipListen: DefaultClipListen,
	}
}

// LocalPath is the project-local config filename mole looks for in the
// current working directory.
const LocalPath = "mole.yaml"

// GlobalPath returns the per-user config file location for the current
// OS: ~/.config/mole/config.yaml on Unix (honouring XDG_CONFIG_HOME),
// %APPDATA%\mole\config.yaml on Windows. This is the single source of
// truth for where `mole init -global` writes and where `mole up` looks
// when there is no project-local config.
func GlobalPath() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("APPDATA")
		if base == "" {
			base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
		}
		return filepath.Join(base, "mole", "config.yaml")
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "mole", "config.yaml")
}

// StateDir returns the per-user directory for mole's runtime state
// (pidfile, background log): ~/.local/state/mole on Unix (honouring
// XDG_STATE_HOME), %LOCALAPPDATA%\mole on Windows. The directory is not
// created here — callers do that when they need to write.
func StateDir() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
		return filepath.Join(base, "mole")
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "mole")
}

// SearchPaths returns the ordered list of config locations mole checks
// when the user does not pass an explicit -config: project-local first
// (so a repo's mole.yaml wins), then the user-global config.
func SearchPaths() []string {
	paths := []string{LocalPath}
	if g := GlobalPath(); g != "" {
		paths = append(paths, g)
	}
	return paths
}

// Find returns the first existing path from SearchPaths, or "" if none
// exist (in which case Load("") yields just the defaults).
func Find() string {
	for _, p := range SearchPaths() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Load reads a YAML config file from path and merges it on top of the
// defaults. If path is empty or the file doesn't exist, only defaults
// are returned (no error).
func Load(path string) (*Config, error) {
	cfg := Default()

	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != nil {
		if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
	} else {
		return nil, fmt.Errorf("parse config %q: multiple YAML documents are not supported", path)
	}

	// Keep existing clip_url-only configurations usable after the safer
	// loopback default was introduced. An explicit clip_listen, including
	// loopback, always wins over this compatibility fallback.
	var clipConfig struct {
		ClipURL    string  `yaml:"clip_url"`
		ClipListen *string `yaml:"clip_listen"`
	}
	if err := yaml.Unmarshal(data, &clipConfig); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if strings.TrimSpace(clipConfig.ClipURL) != "" {
		listen, err := clipListenForURL(clipConfig.ClipURL)
		if err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
		if clipConfig.ClipListen == nil {
			cfg.ClipListen = listen
		}
	}

	for _, d := range cfg.Domains {
		if err := ValidateDomain(d); err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
	}
	for _, p := range cfg.DomainPorts {
		if p < 1 || p > 65535-meshdns.PortShift {
			return nil, fmt.Errorf("parse config %q: domain_ports: %d out of range (1-%d)", path, p, 65535-meshdns.PortShift)
		}
	}
	if err := cfg.Transfer.Validate(); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	for name, n := range cfg.Nodes {
		if err := ValidNodeName(name); err != nil {
			return nil, fmt.Errorf("parse config %q: nodes: %w", path, err)
		}
		if err := n.Validate(); err != nil {
			return nil, fmt.Errorf("parse config %q: nodes.%s: %w", path, name, err)
		}
	}
	seen := make(map[int]bool, len(cfg.Push))
	for _, r := range cfg.Push {
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
		if seen[r.Local] {
			return nil, fmt.Errorf("parse config %q: push: duplicate local port %d", path, r.Local)
		}
		seen[r.Local] = true
	}

	return cfg, nil
}

func clipListenForURL(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", nil
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
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", errors.New("invalid clip URL: port must be between 1 and 65535")
	}
	return net.JoinHostPort(parsed.Hostname(), port), nil
}

// Save writes cfg back to path as YAML, preserving any comments and
// unknown keys already in the file. Without fields, cfg is a full snapshot:
// existing keys are updated even when their new value is empty, false, or
// zero. Optional field names select a partial update, including zero values.
//
// If the file does not exist, Save creates it with a short header
// comment and a marshaled default.
func Save(path string, cfg *Config, fields ...string) error {
	if path == "" {
		return fmt.Errorf("config: empty path")
	}
	if cfg == nil {
		return errors.New("config: nil config")
	}
	selected := make(map[string]bool, len(fields))
	for _, field := range fields {
		if !configFields[field] {
			return fmt.Errorf("config: unknown field %q", field)
		}
		selected[field] = true
	}

	// Ensure the parent directory exists before we touch anything.
	// Save can be the first write to a fresh path.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	if data, err := os.ReadFile(path); err == nil {
		var node yaml.Node
		if uerr := yaml.Unmarshal(data, &node); uerr != nil {
			return fmt.Errorf("parse existing %q: %w", path, uerr)
		}
		// Update top-level fields in place. yaml.v3 represents a
		// document as a sequence of nodes; the actual mapping
		// lives at node.Content[0]. Each pair in Content is
		// [key, value, key, value, ...].
		var keptComments []byte
		if len(node.Content) == 0 && len(bytes.TrimSpace(stripYAMLComments(data))) == 0 {
			// An empty file, or one with only comments: start a mapping in
			// it, and keep whatever comments it had on top.
			if t := bytes.TrimSpace(data); len(t) > 0 {
				keptComments = append(t, '\n')
			}
			node = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
		}
		if len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
			return fmt.Errorf("config %q: top-level is not a mapping", path)
		}
		if err := updateMapping(node.Content[0], cfg, selected); err != nil {
			return err
		}
		encoded, err := yaml.Marshal(&node)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		// yaml.v3 emits two leading "---" bytes that we don't want
		// at the top of a config file. Drop them.
		out := append(keptComments, bytes.TrimPrefix(encoded, []byte("---\n"))...)
		return atomicWriteFile(path, out, 0o644)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %q: %w", path, err)
	}

	// File doesn't exist: create with a header comment.
	header := "# mole configuration\n# Generated by `mole ports add`; edit freely.\n"
	var body []byte
	var err error
	if len(selected) == 0 {
		body, err = yaml.Marshal(cfg)
	} else {
		mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if err := updateMapping(mapping, cfg, selected); err != nil {
			return err
		}
		body, err = yaml.Marshal(mapping)
	}
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return atomicWriteFile(path, []byte(header+string(body)), 0o644)
}

// stripYAMLComments drops comment-only and blank lines, to tell a file that
// holds nothing from one that holds something.
func stripYAMLComments(data []byte) []byte {
	var out []byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if t := bytes.TrimSpace(line); len(t) > 0 && t[0] != '#' {
			out = append(out, line...)
			out = append(out, '\n')
		}
	}
	return out
}

var configFields = map[string]bool{
	"remote": true, "ports": true, "push": true, "domains": true, "domain_ports": true, "auto_discover": true,
	"discover_ports": true, "exclude_ports": true, "admin_addr": true,
	"log_level": true, "ssh_port": true, "insecure": true,
	"clip_url": true, "clip_listen": true, "clip_interval_ms": true,
	"transfer": true, "nodes": true,
}

func updateMapping(m *yaml.Node, cfg *Config, selected map[string]bool) error {
	// Build a lookup from yaml key to (index-in-Content) so we
	// can update without scanning twice.
	index := map[string]int{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		key := m.Content[i].Value
		index[key] = i + 1
	}

	// helper: write a field into the mapping. If key exists, replace
	// the value node in place (keeping the key node's comments).
	// If not, append a new key/value pair at the end.
	write := func(yamlKey string, val any, nonzero bool) error {
		if len(selected) > 0 && !selected[yamlKey] {
			return nil
		}
		idx, exists := index[yamlKey]
		if !exists && !nonzero && !selected[yamlKey] {
			return nil
		}
		var newVal yaml.Node
		if err := newVal.Encode(val); err != nil {
			return fmt.Errorf("encode %s: %w", yamlKey, err)
		}
		if exists {
			old := m.Content[idx]
			newVal.HeadComment = old.HeadComment
			newVal.LineComment = old.LineComment
			newVal.FootComment = old.FootComment
			m.Content[idx] = &newVal
			return nil
		}
		// Append: key node + new value node.
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: yamlKey}
		m.Content = append(m.Content, keyNode, &newVal)
		return nil
	}
	values := []struct {
		key     string
		value   any
		nonzero bool
	}{
		{"remote", cfg.Remote, cfg.Remote != ""},
		{"ports", nonNilPorts(cfg.Ports), len(cfg.Ports) > 0},
		{"push", nonNilPush(cfg.Push), len(cfg.Push) > 0},
		{"domains", nonNilStrings(cfg.Domains), len(cfg.Domains) > 0},
		{"domain_ports", nonNilPorts(cfg.DomainPorts), len(cfg.DomainPorts) > 0},
		{"auto_discover", cfg.AutoDiscover, cfg.AutoDiscover},
		{"discover_ports", nonNilPorts(cfg.DiscoverPorts), len(cfg.DiscoverPorts) > 0},
		{"exclude_ports", nonNilPorts(cfg.ExcludePorts), len(cfg.ExcludePorts) > 0},
		{"admin_addr", cfg.AdminAddr, cfg.AdminAddr != ""},
		{"log_level", cfg.LogLevel, cfg.LogLevel != ""},
		{"ssh_port", cfg.SSHPort, cfg.SSHPort != 0},
		{"insecure", cfg.Insecure, cfg.Insecure},
		{"clip_url", cfg.ClipURL, cfg.ClipURL != ""},
		{"clip_listen", cfg.ClipListen, cfg.ClipListen != ""},
		{"clip_interval_ms", cfg.ClipIntervalMs, cfg.ClipIntervalMs != 0},
		{"transfer", cfg.Transfer, cfg.Transfer != (TransferConfig{})},
		{"nodes", nonNilNodes(cfg.Nodes), len(cfg.Nodes) > 0},
	}
	for _, item := range values {
		if err := write(item.key, item.value, item.nonzero); err != nil {
			return err
		}
	}
	return nil
}

func nonNilPorts(ports []int) []int {
	if ports == nil {
		return []int{}
	}
	return ports
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// DomainPortsOrDefault returns DomainPorts, or 80 and 443 when unset.
func (c *Config) DomainPortsOrDefault() []int {
	if len(c.DomainPorts) == 0 {
		return []int{80, 443}
	}
	return c.DomainPorts
}

// ValidateDomain accepts plain DNS suffixes (letters, digits, '-', '.');
// the value ends up in a systemd unit, so nothing else is allowed.
func ValidateDomain(d string) error {
	if d == "" || len(d) > 253 || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") || strings.Contains(d, "..") {
		return fmt.Errorf("domains: invalid domain %q", d)
	}
	for _, r := range d {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return fmt.Errorf("domains: invalid domain %q (lowercase letters, digits, '-' and '.' only)", d)
		}
	}
	return nil
}
func nonNilNodes(n map[string]NodeConfig) map[string]NodeConfig {
	if n == nil {
		return map[string]NodeConfig{}
	}
	return n
}

func nonNilPush(rules []PushRule) []PushRule {
	if rules == nil {
		return []PushRule{}
	}
	return rules
}

// atomicWriteFile writes data to path via a temp file + rename so
// concurrent readers never see a half-written config. POSIX rename
// is atomic within a directory.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mole-yaml-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// ParsePorts converts a comma-separated port list ("3000,5173,8080")
// into a slice of ints. Empty entries and invalid numbers are skipped.
func ParsePorts(s string) []int {
	if s == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// MergePorts returns the union of a and b, preserving order and removing
// duplicates. Elements from a come first.
func MergePorts(a, b []int) []int {
	seen := make(map[int]bool, len(a)+len(b))
	out := make([]int, 0, len(a)+len(b))
	for _, p := range a {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range b {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
