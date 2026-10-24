package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/routes"
)

// The Domains tab: the domain names live under, the named routes inside it,
// and the one-time DNS install. Changes go through the `mole` CLI so it stays
// the single writer of the config.

// RouteView is one route as the tab shows it.
type RouteView struct {
	Name   string   `json:"name"`
	Hosts  []string `json:"hosts"`
	Target string   `json:"target"`
	URL    string   `json:"url"` // where a browser reaches it right now
}

// DNSState says whether the OS resolves the domain through mole.
type DNSState struct {
	Supported bool   `json:"supported"`
	Installed bool   `json:"installed"`
	Detail    string `json:"detail"`
}

// DomainInfo is what the Domains tab renders.
type DomainInfo struct {
	Remote    string      `json:"remote"` // the primary remote: where routes go unless a node is named
	Domains   []string    `json:"domains"`
	Routes    []RouteView `json:"routes"`
	RoutePort int         `json:"routePort"`
	Running   bool        `json:"running"` // the daemon answers: edits apply live
	Warning   string      `json:"warning"` // why the domain is a poor choice, or ""
	DNS       DNSState    `json:"dns"`
	Error     string      `json:"error,omitempty"`
}

// Install locations, mirrored from `mole dns` (cmd/mole/dns*.go).
const (
	macResolverDir    = "/etc/resolver"
	macResolverMarker = "# managed by mole"
	macAnchorFile     = "/etc/pf.anchors/mole"
	linuxNftFile      = "/etc/mole-dns.nft"
	linuxUnitFile     = "/etc/systemd/system/mole-dns.service"
)

// dnsStateAt inspects the install below root ("" for the real filesystem). An
// install whose port redirect points somewhere other than routePort counts as
// missing: it was made before route_port changed and sends port 80 nowhere.
func dnsStateAt(goos, root string, domains []string, routePort int) DNSState {
	switch goos {
	case "darwin":
		st := DNSState{Supported: true}
		if len(domains) == 0 {
			st.Detail = "no domain set"
			return st
		}
		var missing []string
		for _, d := range domains {
			b, err := os.ReadFile(filepath.Join(root, macResolverDir, d))
			if err != nil || !strings.HasPrefix(string(b), macResolverMarker) {
				missing = append(missing, "/etc/resolver/"+d)
			}
		}
		rules, err := os.ReadFile(filepath.Join(root, macAnchorFile))
		switch {
		case err != nil:
			missing = append(missing, "pf redirect")
		case !strings.Contains(string(rules), fmt.Sprintf("-> 127.0.0.1 port %d\n", routePort)):
			missing = append(missing, fmt.Sprintf("pf redirect to port %d (the installed one points at another port)", routePort))
		}
		st.Installed = len(missing) == 0
		if !st.Installed {
			st.Detail = "missing: " + strings.Join(missing, ", ")
		}
		return st
	case "linux":
		st := DNSState{Supported: true}
		if len(domains) == 0 {
			st.Detail = "no domain set"
			return st
		}
		nft, nftErr := os.ReadFile(filepath.Join(root, linuxNftFile))
		_, unitErr := os.Stat(filepath.Join(root, linuxUnitFile))
		switch {
		case nftErr != nil || unitErr != nil:
			st.Detail = "mole-dns.service is not installed"
		case !strings.Contains(string(nft), fmt.Sprintf("dnat to 127.0.0.1:%d\n", routePort)):
			st.Detail = fmt.Sprintf("the installed redirect does not point at port %d", routePort)
		default:
			st.Installed = true
		}
		return st
	}
	return DNSState{Detail: "not supported on " + goos + " yet: use the port URLs"}
}

// routeURL is where a browser reaches a route: no port once the DNS install
// has redirected port 80, the route proxy's own port before that.
func routeURL(host string, installed bool, routePort int) string {
	if installed {
		return "http://" + host
	}
	return fmt.Sprintf("http://%s:%d", host, routePort)
}

// GetDomains reports the domain, its routes (live when the daemon is up) and
// the state of the DNS install.
func (m *MoleService) GetDomains() DomainInfo {
	cfg, _, err := loadConfig()
	if err != nil {
		return DomainInfo{Error: err.Error()}
	}
	info := DomainInfo{Remote: cfg.Remote, Domains: cfg.Domains, RoutePort: cfg.RoutePortOrDefault()}
	if info.Domains == nil {
		info.Domains = []string{}
	}
	if len(cfg.Domains) > 0 {
		info.Warning = config.DomainAdvice(cfg.Domains[0])
	}
	info.DNS = dnsStateAt(runtime.GOOS, "", cfg.Domains, info.RoutePort)

	list := routes.NewTable(cfg.Domains, cfg.Routes).List()
	if cfg.AdminAddr != "" {
		if live, ok := m.liveRoutes(cfg.AdminAddr); ok {
			info.Running = true
			list = list[:0]
			for _, r := range live {
				list = append(list, routes.Route{Name: r.Name, Hosts: r.Hosts, Target: r.Target})
			}
		}
	}
	info.Routes = make([]RouteView, 0, len(list))
	for _, r := range list {
		v := RouteView{Name: r.Name, Hosts: r.Hosts, Target: r.Target}
		if len(r.Hosts) > 0 {
			v.URL = routeURL(r.Hosts[0], info.DNS.Installed, info.RoutePort)
		}
		info.Routes = append(info.Routes, v)
	}
	return info
}

// liveRoutes asks the daemon; ok is false when it is down or predates routes.
func (m *MoleService) liveRoutes(adminAddr string) (list []routes.Route, ok bool) {
	resp, err := m.client.Get("http://" + adminAddr + "/status")
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var body struct {
		Info struct {
			Routes *[]routes.Route `json:"routes"`
		} `json:"info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, false
	}
	if body.Info.Routes == nil {
		return nil, true // daemon up, no route proxy (no domain yet)
	}
	return *body.Info.Routes, true
}

// SetDomain sets the domain routes live under. A running daemon is restarted:
// the DNS stub and the route table are built at start.
func (m *MoleService) SetDomain(name string) error {
	if err := m.mole("domain", "set", name); err != nil {
		return err
	}
	if cfg, _, err := loadConfig(); err == nil && cfg.AdminAddr != "" {
		if _, up := m.liveRoutes(cfg.AdminAddr); up {
			return m.mole("restart")
		}
	}
	return nil
}

// AddRoute names a service on the remote: target is PORT or HOST:PORT.
func (m *MoleService) AddRoute(name, target string) error {
	return m.mole("route", "add", name, target)
}

// RemoveRoute deletes a route.
func (m *MoleService) RemoveRoute(name string) error {
	return m.mole("route", "rm", name)
}

// OpenRoute opens a route in the browser. It takes the route's name, not a
// URL, so the page cannot make the app open an arbitrary address.
func (m *MoleService) OpenRoute(name string) error {
	for _, r := range m.GetDomains().Routes {
		if r.Name == name && r.URL != "" {
			return openExternal(r.URL)
		}
	}
	return fmt.Errorf("no route named %q", name)
}

func openExternal(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// InstallDNS points the OS at mole's DNS stub for the domain. It needs root:
// the system's own authentication dialog asks for it.
func (m *MoleService) InstallDNS() error { return m.dnsAction("install") }

// UninstallDNS undoes InstallDNS.
func (m *MoleService) UninstallDNS() error { return m.dnsAction("uninstall") }

func (m *MoleService) dnsAction(action string) error {
	bin, err := moleBin()
	if err != nil {
		return err
	}
	// root does not see the user's config: name it explicitly.
	_, path, err := loadConfig()
	if err != nil {
		return err
	}
	argv := []string{bin, "dns", action, "-config", path}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd, err := privilegedCommand(ctx, runtime.GOOS, argv)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if strings.Contains(msg, "User canceled") {
			return fmt.Errorf("cancelled")
		}
		return fmt.Errorf("mole dns %s: %s", action, msg)
	}
	return nil
}

// privilegedCommand wraps argv so it runs as root, asking through the
// system's own dialog: osascript on macOS, polkit on Linux.
func privilegedCommand(ctx context.Context, goos string, argv []string) (*exec.Cmd, error) {
	switch goos {
	case "darwin":
		script := `do shell script ` + appleScriptString(shellJoin(argv)) + ` with administrator privileges`
		return exec.CommandContext(ctx, "osascript", "-e", script), nil
	case "linux":
		if _, err := exec.LookPath("pkexec"); err != nil {
			return nil, fmt.Errorf("pkexec not found: run `sudo mole dns install -config PATH` in a terminal")
		}
		return exec.CommandContext(ctx, "pkexec", argv...), nil
	}
	return nil, fmt.Errorf("installing the DNS routing is not supported on %s yet", goos)
}

// shellJoin quotes each argument for /bin/sh.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// appleScriptString renders s as an AppleScript string literal.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
