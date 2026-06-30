// Package config defines the runtime configuration for mole
// and helpers to load it from YAML and merge CLI overrides.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

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
}

// Default returns a Config populated with sensible defaults.
func Default() *Config {
	return &Config{
		AdminAddr: "127.0.0.1:9999",
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
		LogLevel: "info",
		SSHPort:  22,
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

	return cfg, nil
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
