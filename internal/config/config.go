// Package config defines the runtime configuration for mole
// and helpers to load it from YAML and merge CLI overrides.
package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
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

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
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
	if clipConfig.ClipListen == nil && strings.TrimSpace(clipConfig.ClipURL) != "" {
		listen, err := clipListenForURL(clipConfig.ClipURL)
		if err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
		cfg.ClipListen = listen
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
		return "", fmt.Errorf("invalid clip URL %q: %w", raw, err)
	}
	if parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid clip URL %q: missing host", raw)
	}
	port := parsed.Port()
	if port == "" {
		port = "7777"
	}
	return net.JoinHostPort(parsed.Hostname(), port), nil
}

// Save writes cfg back to path as YAML, preserving any comments and
// non-default keys that were already in the file. The strategy is:
//
//  1. Read the existing file (if any) into a yaml.Node tree.
//  2. Decode the existing tree into a fresh Config so we know what
//     was on disk, then overlay cfg on top.
//  3. Re-encode the resulting tree back to YAML and write it.
//
// This means a Save() round-trip never loses user comments, never
// reorders keys, and never drops fields cfg didn't touch.
// Save writes cfg back to path as YAML, preserving any comments and
// non-default keys that were already in the file. The strategy is:
//
//  1. Read the existing file (if any) into a yaml.Node tree.
//  2. Find the top-level mapping node, then for each non-zero
//     field in cfg, walk into the matching key's child node and
//     update it in place. This preserves every comment, every
//     key order, every format choice the user made.
//  3. yaml.Marshal the updated node and write atomically.
//
// This means a Save() round-trip never loses user comments, never
// reorders keys, and never drops fields cfg didn't touch.
//
// If the file does not exist, Save creates it with a short header
// comment and a marshaled default.
func Save(path string, cfg *Config) error {
	if path == "" {
		return fmt.Errorf("config: empty path")
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
		if len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
			return fmt.Errorf("config %q: top-level is not a mapping", path)
		}
		updateMapping(node.Content[0], cfg)
		encoded, err := yaml.Marshal(&node)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		// yaml.v3 emits two leading "---" bytes that we don't want
		// at the top of a config file. Drop them.
		out := bytes.TrimPrefix(encoded, []byte("---\n"))
		return atomicWriteFile(path, out, 0o644)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %q: %w", path, err)
	}

	// File doesn't exist: create with a header comment.
	header := "# mole configuration\n# Generated by `mole ports add`; edit freely.\n"
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return atomicWriteFile(path, []byte(header+string(body)), 0o644)
}

// updateMapping applies cfg's non-zero fields onto an existing
// yaml.MappingNode in place. For each (key, value) pair in cfg
// whose value is not the zero value, we find the matching key in
// the mapping and replace its value with a freshly-encoded node
// from cfg. Keys that are present in the mapping but not in cfg
// are left untouched (so user-only fields survive).
//
// Caveat: a key in cfg that does NOT exist in the mapping is also
// added. That's the "first time the user runs `mole ports add` on
// a config that didn't have discover_ports:" path.
func updateMapping(m *yaml.Node, cfg *Config) {
	// Build a lookup from yaml key to (index-in-Content) so we
	// can update without scanning twice.
	type slot struct {
		keyIdx int // index in m.Content of the key node
		valIdx int // index of the matching value node
	}
	index := map[string]slot{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		key := m.Content[i].Value
		index[key] = slot{keyIdx: i, valIdx: i + 1}
	}

	// helper: write a field into the mapping. If key exists, replace
	// the value node in place (keeping the key node's comments).
	// If not, append a new key/value pair at the end.
	write := func(yamlKey string, val any) {
		var newVal yaml.Node
		if err := newVal.Encode(val); err != nil {
			return // silently skip; Save's caller will see a missing field
		}
		if s, ok := index[yamlKey]; ok {
			m.Content[s.valIdx] = &newVal
			return
		}
		// Append: key node + new value node.
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: yamlKey}
		m.Content = append(m.Content, keyNode, &newVal)
	}

	if cfg.Remote != "" {
		write("remote", cfg.Remote)
	}
	if len(cfg.Ports) > 0 {
		write("ports", cfg.Ports)
	}
	if cfg.AutoDiscover {
		write("auto_discover", cfg.AutoDiscover)
	}
	if len(cfg.DiscoverPorts) > 0 {
		write("discover_ports", cfg.DiscoverPorts)
	}
	if len(cfg.ExcludePorts) > 0 {
		write("exclude_ports", cfg.ExcludePorts)
	}
	if cfg.AdminAddr != "" {
		write("admin_addr", cfg.AdminAddr)
	}
	if cfg.LogLevel != "" {
		write("log_level", cfg.LogLevel)
	}
	if cfg.SSHPort != 0 {
		write("ssh_port", cfg.SSHPort)
	}
	if cfg.Insecure {
		write("insecure", cfg.Insecure)
	}
	if cfg.ClipURL != "" {
		write("clip_url", cfg.ClipURL)
	}
	if cfg.ClipListen != "" {
		write("clip_listen", cfg.ClipListen)
	}
	if cfg.ClipIntervalMs > 0 {
		write("clip_interval_ms", cfg.ClipIntervalMs)
	}
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
	return os.Rename(tmpName, path)
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
