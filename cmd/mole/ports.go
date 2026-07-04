// mole ports — manage the auto-discover port list in mole.yaml.
//
// The subcommand operates on the active config file (resolved the
// same way `mole up` resolves it: explicit -config, else the
// project-local ./mole.yaml, else the user-global config). All
// edits go through config.Save, which preserves the YAML's
// comments and key order rather than re-rendering the whole file.
//
// Subcommands:
//
//	mole ports add <port>     add a port to discover_ports:
//	mole ports remove <port>  remove a port from discover_ports:
//	mole ports list           print the current discover_ports:
//
// The changes are persisted to disk AND, if `mole up` is running,
// are pushed live to its admin API so the change applies without
// a restart. If the daemon is not running, the YAML is still
// updated; the user will see a "restart mole up" hint and that's
// it. Idempotent throughout: re-adding an already-present port
// is a no-op whether the daemon is up or down.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"github.com/Luqueee/mole/internal/config"
)

func runPorts(args []string) int {
	if len(args) == 0 {
		printPortsUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "add":
		return runPortsAdd(args[1:])
	case "remove", "rm":
		return runPortsRemove(args[1:])
	case "list", "ls":
		return runPortsList(args[1:])
	case "-h", "--help", "help":
		printPortsUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mole ports: unknown subcommand: %s\n\n", args[0])
		printPortsUsage(os.Stderr)
		return 2
	}
}

func printPortsUsage(w *os.File) {
	color := cliColor(w)
	fmt.Fprintf(w, "%s\n\n", cBold("mole ports — manage the auto-discover port list", color))
	fmt.Fprintf(w, "  %s\n", cBold("USAGE", color))
	fmt.Fprintf(w, "    mole ports %s\n", cDim("<add|remove|list> [flags]", color))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", cBold("SUBCOMMANDS", color))
	fmt.Fprintf(w, "    %s  add a port to discover_ports: (idempotent)\n", cGreen("add <port>", color))
	fmt.Fprintf(w, "    %s  remove a port from discover_ports:\n", cGreen("remove <port>", color))
	fmt.Fprintf(w, "    %s  print the current discover_ports: list\n", cGreen("list", color))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", cBold("NOTES", color))
	fmt.Fprintf(w, "    %s\n", cDim("  changes are written to the active mole.yaml but do NOT", color))
	fmt.Fprintf(w, "    %s\n", cDim("  affect a running `mole up` — restart mole to pick them up.", color))
}

// runPortsAdd appends a single port to discover_ports. It refuses
// to add a port that's already in exclude_ports (the user almost
// certainly doesn't want that forwarded) and exits 0 (idempotent)
// if the port is already in discover_ports.
func runPortsAdd(args []string) int {
	fs := flag.NewFlagSet("ports add", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole ports add <port> [flags]

Adds <port> to the discover_ports: list in the active mole.yaml.
Idempotent: if the port is already listed, exits 0 without changes.

Flags:
  -config       path to YAML config (default: ./mole.yaml, then user-global)`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "mole ports add: expected exactly one port argument")
		fs.Usage()
		return 2
	}
	port, err := strconv.Atoi(fs.Arg(0))
	if err != nil || port <= 0 || port > 65535 {
		fmt.Fprintf(os.Stderr, "mole ports add: invalid port %q (expected 1-65535)\n", fs.Arg(0))
		return 2
	}

	resolved, err := resolveOrCreateConfigPath(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}

	cfg, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %s: %v\n", resolved, err)
		return 1
	}

	// Refuse if the port is in exclude_ports — the user almost
	// certainly doesn't want it forwarded. We treat this as a hard
	// error so `mole ports add 22` doesn't silently add a "port that
	// auto-discover would then immediately skip", which would be
	// confusing.
	for _, p := range cfg.ExcludePorts {
		if p == port {
			fmt.Fprintf(os.Stderr, "mole ports add: %d is in exclude_ports; remove it there first if you really want it forwarded\n", port)
			return 1
		}
	}

	// Idempotent: already present → exit 0 without touching the file.
	// The live-add path below is attempted regardless so even an
	// "already present" port can be re-applied to a running daemon
	// (e.g. after `mole up` was restarted and lost the runtime
	// listener).
	for _, p := range cfg.DiscoverPorts {
		if p == port {
			fmt.Printf("%d is already in discover_ports:\n", port)
			tryLiveAdd(cfg, port)
			return 0
		}
	}

	cfg.DiscoverPorts = append(cfg.DiscoverPorts, port)
	sort.Ints(cfg.DiscoverPorts)

	if err := config.Save(resolved, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "mole ports add: write %s: %v\n", resolved, err)
		return 1
	}
	fmt.Printf("added %d to discover_ports: in %s\n", port, resolved)
	tryLiveAdd(cfg, port)
	return 0
}

// tryLiveAdd probes the admin API and, if mole up is alive, POSTs
// the port to /ports/discover so the running daemon picks it up
// without a restart. On any failure (daemon down, network error,
// admin_addr unset) we just print a note and return 0 — the YAML
// was the source of truth and is already saved.
func tryLiveAdd(cfg *config.Config, port int) {
	if cfg.AdminAddr == "" {
		return
	}
	if !daemonLive(cfg.AdminAddr) {
		fmt.Printf("note: mole up not running at %s; restart it to apply the new port\n", cfg.AdminAddr)
		return
	}
	if err := adminPostPort(cfg.AdminAddr, port); err != nil {
		fmt.Fprintf(os.Stderr, "warning: live add failed (%v); the YAML is updated, restart mole up to apply\n", err)
		return
	}
	fmt.Printf("live: mole up is forwarding %d now (no restart needed)\n", port)
}

// daemonLive returns true iff the admin API at addr responds to
// GET /health with 200 OK within a short timeout. We use a tight
// timeout because this is a hot path on every `mole ports` call —
// if the user is on a slow VPN we'd rather skip the live path
// than hang the command.
func daemonLive(addr string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// adminPostPort POSTs {"port": N} to the admin API. Returns nil
// on 201 Created; non-nil on any other status or transport error.
func adminPostPort(addr string, port int) error {
	body := fmt.Sprintf(`{"port":%d}`, port)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post("http://"+addr+"/ports/discover", "application/json", strings.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		return nil
	}
	// Read a bit of the body for a useful error message.
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return fmt.Errorf("admin %s: %s", resp.Status, strings.TrimSpace(string(preview)))
}

// adminDeletePort DELETEs /ports/discover/{port} on the admin API.
func adminDeletePort(addr string, port int) error {
	client := &http.Client{Timeout: 2 * time.Second}
	u := fmt.Sprintf("http://%s/ports/discover/%d", addr, port)
	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return fmt.Errorf("admin %s: %s", resp.Status, strings.TrimSpace(string(preview)))
}

// runPortsRemove drops a port from discover_ports. Idempotent:
// if it's not there, exit 0.
func runPortsRemove(args []string) int {
	fs := flag.NewFlagSet("ports remove", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole ports remove <port> [flags]

Removes <port> from the discover_ports: list. Idempotent: not
present is fine.

Flags:
  -config       path to YAML config (default: ./mole.yaml, then user-global)`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "mole ports remove: expected exactly one port argument")
		fs.Usage()
		return 2
	}
	port, err := strconv.Atoi(fs.Arg(0))
	if err != nil || port <= 0 || port > 65535 {
		fmt.Fprintf(os.Stderr, "mole ports remove: invalid port %q (expected 1-65535)\n", fs.Arg(0))
		return 2
	}

	resolved, err := resolveOrCreateConfigPath(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}

	cfg, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %s: %v\n", resolved, err)
		return 1
	}

	before := len(cfg.DiscoverPorts)
	out := cfg.DiscoverPorts[:0]
	for _, p := range cfg.DiscoverPorts {
		if p != port {
			out = append(out, p)
		}
	}
	cfg.DiscoverPorts = out
	if len(cfg.DiscoverPorts) == before {
		fmt.Printf("%d is not in discover_ports: (no change)\n", port)
		return 0
	}

	if err := config.Save(resolved, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "mole ports remove: write %s: %v\n", resolved, err)
		return 1
	}
	fmt.Printf("removed %d from discover_ports: in %s\n", port, resolved)
	tryLiveRemove(cfg, port)
	return 0
}

// tryLiveRemove is the remove-side counterpart of tryLiveAdd. As
// with add, the YAML is the source of truth; the live call is
// best-effort and failure is non-fatal.
func tryLiveRemove(cfg *config.Config, port int) {
	if cfg.AdminAddr == "" {
		return
	}
	if !daemonLive(cfg.AdminAddr) {
		fmt.Printf("note: mole up not running at %s; no live action needed\n", cfg.AdminAddr)
		return
	}
	if err := adminDeletePort(cfg.AdminAddr, port); err != nil {
		fmt.Fprintf(os.Stderr, "warning: live remove failed (%v); the YAML is updated, restart mole up to apply\n", err)
		return
	}
	fmt.Printf("live: mole up stopped forwarding %d now (no restart needed)\n", port)
}

// runPortsList prints the current discover_ports list, one per
// line, plus the exclude_ports for context. Reads-only.
func runPortsList(args []string) int {
	fs := flag.NewFlagSet("ports list", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole ports list [flags]

Prints the current discover_ports: list, one per line, followed by
the exclude_ports: list.

Flags:
  -config       path to YAML config (default: ./mole.yaml, then user-global)`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resolved := *configPath
	if resolved == "" {
		resolved = config.Find()
	}
	if resolved == "" {
		fmt.Fprintln(os.Stderr, "no mole.yaml found (try `mole init` first)")
		return 1
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %s: %v\n", resolved, err)
		return 1
	}

	fmt.Printf("config: %s\n", resolved)
	fmt.Println("discover_ports:")
	if len(cfg.DiscoverPorts) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, p := range cfg.DiscoverPorts {
			fmt.Printf("  %d\n", p)
		}
	}
	fmt.Println("exclude_ports:")
	if len(cfg.ExcludePorts) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, p := range cfg.ExcludePorts {
			fmt.Printf("  %d\n", p)
		}
	}
	return 0
}

// resolveOrCreateConfigPath returns the path that should be edited.
// It honours -config (if set), else the standard search order. If
// the resolved path doesn't exist, it creates a fresh config with
// the defaults from config.Default() — that way `mole ports add`
// on a fresh checkout doesn't fail with "no config"; it creates
// a minimal one and adds the port.
func resolveOrCreateConfigPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	found := config.Find()
	if found != "" {
		return found, nil
	}
	// Nothing found — create a fresh config in cwd.
	return "./mole.yaml", nil
}

// errNotFound is a sentinel used in tests. Not currently
// returned by production code, but kept here so the helper above
// can grow a "not found" branch in the future without churning
// callers.
var errNotFound = errors.New("config: no mole.yaml found")
