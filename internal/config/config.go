// Package config defines the runtime configuration for fallback-proxy
// and helpers to load it from YAML and merge CLI overrides.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds the runtime configuration for fallback-proxy.
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

	// AdminAddr is the address of the local admin HTTP API.
	// Set to empty string to disable.
	AdminAddr string `yaml:"admin_addr"`

	// LogLevel controls verbosity: "debug", "info", "warn", "error".
	LogLevel string `yaml:"log_level"`

	// SSHPort is the port on the remote to connect to for SSH (default 22).
	SSHPort int `yaml:"ssh_port"`
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
		LogLevel: "info",
		SSHPort:  22,
	}
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