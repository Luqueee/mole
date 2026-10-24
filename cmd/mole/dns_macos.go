// mole dns on macOS: no systemd-resolved, no nftables, no mole0 link.
//
//	sudo mole dns install     resolver file per domain, lo0 alias, pf redirect,
//	                          LaunchDaemon that re-applies them at boot
//	sudo mole dns uninstall   remove all of it
//
// /etc/resolver/<domain> sends only that domain to mole's stub on
// 127.0.0.1:5300; the stub answers route names with meshdns.RouteIP. That
// address is aliased on lo0 and pf redirects its port 80 to the Host proxy, so
// http://a.mole.lan needs no port and plain localhost:80 is left alone. Only
// named routes work here: forward-by-name needs a whole 127.77.0.0/16 aliased.
//
// The render functions are pure so they are tested on every OS.
package main

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/meshdns"
)

const (
	macResolverDir    = "/etc/resolver"
	macResolverMarker = "# managed by mole"
	macAnchorFile     = "/etc/pf.anchors/mole"
	// Apple's /etc/pf.conf evaluates rdr-anchor "com.apple/*", so an anchor
	// below it needs no edit to the system ruleset.
	macAnchorName = "com.apple/mole"
	macLabel      = "dev.mole.dns"
	macPlistPath  = "/Library/LaunchDaemons/" + macLabel + ".plist"
)

// macResolverFile is the /etc/resolver/<domain> body.
func macResolverFile() string {
	host, port, _ := strings.Cut(meshdns.LoopbackStubAddr, ":")
	return macResolverMarker + "\nnameserver " + host + "\nport " + port + "\n"
}

// macPfRules redirects the route IP's port 80 to the Host proxy.
func macPfRules(routePort int) string {
	return "rdr pass on lo0 inet proto tcp from any to " + meshdns.RouteIP +
		" port 80 -> 127.0.0.1 port " + strconv.Itoa(routePort) + "\n"
}

// macStartScript makes the route IP reachable and loads the redirect. It is
// run once at install and by the LaunchDaemon at every boot; both steps are
// idempotent.
func macStartScript() string {
	return "ifconfig lo0 alias " + meshdns.RouteIP + " netmask 255.255.255.255; " +
		"pfctl -a " + macAnchorName + " -f " + macAnchorFile + " && pfctl -E"
}

func macPlist() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + macLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>` + html.EscapeString(macStartScript()) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
}

func macFail(err error) int {
	fmt.Fprintln(os.Stderr, "mole dns:", err)
	return 1
}

func runDNSMac(action, configPath string) int {
	if action == "uninstall" {
		return macUninstall()
	}
	cfg, err := config.Load(resolveConfigPathReadOnly(configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if len(cfg.Domains) == 0 {
		fmt.Fprintln(os.Stderr, "mole dns: no domains: in the config (`mole domain set mole.lan`)")
		return 1
	}

	if err := os.MkdirAll(filepath.Dir(macAnchorFile), 0o755); err != nil {
		return macFail(err)
	}
	if err := os.WriteFile(macAnchorFile, []byte(macPfRules(cfg.RoutePortOrDefault())), 0o644); err != nil {
		return macFail(err)
	}
	// Apply first: if pf or ifconfig refuse, nothing else has been touched.
	if out, err := exec.Command("/bin/sh", "-c", macStartScript()).CombinedOutput(); err != nil {
		_ = os.Remove(macAnchorFile)
		return macFail(fmt.Errorf("applying the pf redirect: %v\n%s", err, out))
	}

	if err := os.MkdirAll(macResolverDir, 0o755); err != nil {
		return macFail(err)
	}
	for _, d := range cfg.Domains {
		if err := writeResolver(d); err != nil {
			return macFail(err)
		}
	}
	// A domain that was changed since the last install must not stay routed.
	pruneResolvers(cfg.Domains)

	if err := os.WriteFile(macPlistPath, []byte(macPlist()), 0o644); err != nil {
		return macFail(err)
	}
	_ = exec.Command("launchctl", "bootout", "system/"+macLabel).Run() // not loaded is fine
	if out, err := exec.Command("launchctl", "bootstrap", "system", macPlistPath).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: LaunchDaemon not loaded (%v: %s); the redirect will not survive a reboot\n", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("dscacheutil", "-flushcache").Run()
	_ = exec.Command("killall", "-HUP", "mDNSResponder").Run()

	fmt.Printf("routing %s to %s; %s:80 redirected to the route proxy on port %d\n",
		strings.Join(cfg.Domains, ", "), meshdns.LoopbackStubAddr, meshdns.RouteIP, cfg.RoutePortOrDefault())
	return 0
}

func writeResolver(domain string) error {
	path := filepath.Join(macResolverDir, domain)
	if old, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(old), macResolverMarker) {
		return fmt.Errorf("%s exists and is not managed by mole; not overwriting it", path)
	}
	return os.WriteFile(path, []byte(macResolverFile()), 0o644)
}

// pruneResolvers removes resolver files mole wrote for domains no longer
// configured. Files without mole's marker are never touched.
func pruneResolvers(keep []string) {
	kept := map[string]bool{}
	for _, d := range keep {
		kept[d] = true
	}
	entries, _ := os.ReadDir(macResolverDir)
	for _, e := range entries {
		if kept[e.Name()] {
			continue
		}
		path := filepath.Join(macResolverDir, e.Name())
		if b, err := os.ReadFile(path); err == nil && strings.HasPrefix(string(b), macResolverMarker) {
			_ = os.Remove(path)
		}
	}
}

func macUninstall() int {
	_ = exec.Command("launchctl", "bootout", "system/"+macLabel).Run()
	for _, p := range []string{macPlistPath, macAnchorFile} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return macFail(err)
		}
	}
	_ = exec.Command("pfctl", "-a", macAnchorName, "-F", "all").Run()
	_ = exec.Command("ifconfig", "lo0", "-alias", meshdns.RouteIP).Run()
	pruneResolvers(nil)
	_ = exec.Command("dscacheutil", "-flushcache").Run()
	fmt.Println("removed the resolver files, the pf redirect, the lo0 alias and", macPlistPath)
	return 0
}
