// mole route / mole domain — name the services on the remote.
//
//	mole domain set mole.lan     names live under mole.lan
//	mole route add a 3000        a.mole.lan -> 127.0.0.1:3000 on the remote
//	mole route add api 10.0.0.5:8080
//	mole route add admin https://admin.corp    a name the remote resolves
//	mole route rm a
//	mole route ls
//
// A Host-routing proxy on route_port serves them (see internal/routes), and
// the DNS stub answers 127.0.0.1 for their names; `mole dns install` points
// the OS at the stub.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/routes"
	"github.com/Luqueee/mole/internal/tunnel"
)

// routeManager is the daemon's admin.RouteController: it changes the live
// table. Persisting to the config is the CLI's job, as with push.
type routeManager struct {
	table   *routes.Table
	hasNode func(name string) bool // nodes can be added while the daemon runs
}

func (m *routeManager) SetRoute(name, target string) error {
	if err := config.ValidateRouteName(name); err != nil {
		return fmt.Errorf("%w: %v", admin.ErrInvalid, err)
	}
	norm, err := config.NormalizeRouteTarget(target)
	if err != nil {
		return fmt.Errorf("%w: %v", admin.ErrInvalid, err)
	}
	if node := config.RouteNode(norm); node != "" && !m.hasNode(node) {
		return fmt.Errorf("%w: unknown node %q (register it with `mole node add`)", admin.ErrInvalid, node)
	}
	m.table.Set(name, norm)
	return nil
}

func (m *routeManager) DeleteRoute(name string) error {
	m.table.Delete(name)
	return nil
}

func (m *routeManager) ListRoutes() []admin.RouteInfo {
	list := m.table.List()
	out := make([]admin.RouteInfo, len(list))
	for i, r := range list {
		out[i] = admin.RouteInfo{Name: r.Name, Hosts: r.Hosts, Target: r.Target}
	}
	return out
}

func runRoute(args []string) int {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole route add NAME TARGET [-config path]
       mole route rm NAME
       mole route ls

NAME is one label: with 'domains: [mole.lan]', "a" is reachable as a.mole.lan.
TARGET is where the remote reaches the service:
  3000                  port 3000 on the remote itself
  10.0.0.5:8080         a host the remote sees
  admin.corp            a name the remote resolves (HTTP, port 80)
  https://admin.corp    same over TLS (port 443; :PORT overrides either)
  https+insecure://x    TLS without checking the certificate: for a service with
                        a self-signed certificate or a CA this machine does not trust
To go through another registered node, prefix it: @dockers:3000 is that node's
port 3000, @dockers/db:5432 a host that node sees (each node gets its own
SSH tunnel, opened on first use).
Routes are plain HTTP, picked by the Host header, so many names share one
local port (route_port, default 7080; mole dns install can redirect 80 to it).`)
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	switch sub, rest := fs.Arg(0), fs.Args()[1:]; sub {
	case "add":
		if len(rest) != 2 {
			fs.Usage()
			return 2
		}
		return routeAdd(*configPath, rest[0], rest[1])
	case "rm", "remove":
		if len(rest) != 1 {
			fs.Usage()
			return 2
		}
		return routeRemove(*configPath, rest[0])
	case "ls", "list":
		return routeList(*configPath)
	default:
		fs.Usage()
		return 2
	}
}

func routeAdd(configPath, name, target string) int {
	if err := config.ValidateRouteName(name); err != nil {
		fmt.Fprintln(os.Stderr, "mole route:", err)
		return 2
	}
	norm, err := config.NormalizeRouteTarget(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole route:", err)
		return 2
	}
	resolved, cfg, code := loadForWrite(configPath)
	if code != 0 {
		return code
	}
	if len(cfg.Domains) == 0 {
		fmt.Fprintln(os.Stderr, "mole route: no domain yet; run `mole domain set mole.lan` first")
		return 1
	}
	if node := config.RouteNode(norm); node != "" {
		if _, ok := cfg.Nodes[node]; !ok {
			fmt.Fprintf(os.Stderr, "mole route: unknown node %q (register it with `mole node add`)\n", node)
			return 1
		}
	}
	if cfg.Routes == nil {
		cfg.Routes = map[string]string{}
	}
	cfg.Routes[name] = norm
	if err := config.Save(resolved, cfg, "routes"); err != nil {
		fmt.Fprintf(os.Stderr, "mole route: write %s: %v\n", resolved, err)
		return 1
	}
	fmt.Printf("route %s.%s -> remote %s saved in %s\n", name, cfg.Domains[0], norm, resolved)
	if cfg.AdminAddr == "" || !daemonLive(cfg.AdminAddr) {
		return 0
	}
	body, _ := json.Marshal(map[string]string{"name": name, "target": norm})
	if err := adminDo(http.MethodPost, cfg.AdminAddr, "/routes", string(body), http.StatusCreated); err != nil {
		fmt.Fprintf(os.Stderr, "warning: live route failed (%v); restart mole up to apply\n", err)
		return 1
	}
	fmt.Printf("live: http://%s.%s:%d\n", name, cfg.Domains[0], cfg.RoutePortOrDefault())
	return 0
}

func routeRemove(configPath, name string) int {
	resolved, cfg, code := loadForWrite(configPath)
	if code != 0 {
		return code
	}
	if _, ok := cfg.Routes[name]; ok {
		delete(cfg.Routes, name)
		if err := config.Save(resolved, cfg, "routes"); err != nil {
			fmt.Fprintf(os.Stderr, "mole route: write %s: %v\n", resolved, err)
			return 1
		}
		fmt.Printf("removed route %s from %s\n", name, resolved)
	} else {
		fmt.Printf("%s is not in routes: (no change)\n", name)
	}
	if cfg.AdminAddr != "" && daemonLive(cfg.AdminAddr) {
		if err := adminDo(http.MethodDelete, cfg.AdminAddr, "/routes/"+name, "", http.StatusOK); err != nil {
			fmt.Fprintf(os.Stderr, "warning: live remove failed (%v); restart mole up to apply\n", err)
			return 1
		}
	}
	return 0
}

func routeList(configPath string) int {
	cfg, err := config.Load(resolveConfigPathReadOnly(configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if len(cfg.Routes) == 0 {
		fmt.Println("no routes")
		return 0
	}
	if len(cfg.Domains) == 0 {
		fmt.Println("routes are set but no domain is: `mole domain set mole.lan`")
	}
	list := routes.NewTable(cfg.Domains, cfg.Routes).List()
	for _, r := range list {
		fmt.Printf("%s -> %s  http://%s:%d\n", strings.Join(r.Hosts, ", "), r.Target, firstOr(r.Hosts, r.Name), cfg.RoutePortOrDefault())
	}
	return 0
}

func firstOr(s []string, def string) string {
	if len(s) > 0 {
		return s[0]
	}
	return def
}

func runDomain(args []string) int {
	fs := flag.NewFlagSet("domain", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole domain set NAME [-config path]
       mole domain show

Sets the domain routes live under (a route "a" becomes a.NAME). Any valid DNS
suffix works; .lan and .test are safe choices. Restart mole up to apply, then
run `+"`sudo mole dns install`"+` once so the OS resolves it.`)
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	switch {
	case fs.NArg() == 1 && fs.Arg(0) == "show":
		cfg, err := config.Load(resolveConfigPathReadOnly(*configPath))
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			return 1
		}
		if len(cfg.Domains) == 0 {
			fmt.Println("no domain set")
		}
		for _, d := range cfg.Domains {
			fmt.Println(d)
		}
		return 0
	case fs.NArg() == 2 && fs.Arg(0) == "set":
		name := strings.ToLower(strings.Trim(fs.Arg(1), "."))
		if err := config.ValidateDomain(name); err != nil {
			fmt.Fprintln(os.Stderr, "mole domain:", err)
			return 2
		}
		resolved, cfg, code := loadForWrite(*configPath)
		if code != 0 {
			return code
		}
		cfg.Domains = []string{name}
		if err := config.Save(resolved, cfg, "domains"); err != nil {
			fmt.Fprintf(os.Stderr, "mole domain: write %s: %v\n", resolved, err)
			return 1
		}
		fmt.Printf("domain %s saved in %s\n", name, resolved)
		if w := config.DomainAdvice(name); w != "" {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		fmt.Println("restart mole up to apply, then `sudo mole dns install` once")
		return 0
	}
	fs.Usage()
	return 2
}

// loadForWrite resolves the config a command will modify, creating it when
// missing, and loads it.
func loadForWrite(configPath string) (string, *config.Config, int) {
	resolved, err := resolveOrCreateConfigPath(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return "", nil, 1
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %s: %v\n", resolved, err)
		return "", nil, 1
	}
	return resolved, cfg, 0
}

// nodeConfigured reports whether name is a node in the config file.
func nodeConfigured(configPath, name string) bool {
	cfg, err := config.Load(configPath)
	if err != nil {
		return false
	}
	_, ok := cfg.Nodes[name]
	return ok
}

// openNodeTunnel opens the SSH tunnel for a registered node and keeps it
// alive until ctx is done. The node is read from the config at each open, so
// one added after the daemon started works without a restart.
func openNodeTunnel(ctx context.Context, configPath, name string, log *slog.Logger) (routes.NodeConn, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	n, ok := cfg.Nodes[name]
	if !ok {
		return nil, fmt.Errorf("unknown node %q (register it with `mole node add`)", name)
	}
	port := n.SSHPort
	if port == 0 {
		port = cfg.SSHPort
	}
	rem, err := tunnel.ResolveRemote(n.Remote, port)
	if err != nil {
		return nil, err
	}
	m, err := tunnel.New(rem, n.Insecure, log)
	if err != nil {
		return nil, err
	}
	go m.Watch(ctx, 10*time.Second)
	return m, nil
}
