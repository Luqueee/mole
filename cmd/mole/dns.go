// mole dns — route the configured domains to mole's DNS stub (Linux,
// systemd-resolved + nftables). One-time, needs root:
//
//	sudo mole dns install     create the mole0 dummy link, nft rules, boot unit
//	sudo mole dns uninstall   remove all three
//
// Only the configured domains are routed to mole0 (DefaultRoute=no), so the
// DNS of the active connection is never touched. Connections to the virtual
// IPs (127.77.0.0/16) on a domain port are redirected by nftables to
// port+meshdns.PortShift, where mole listens: that way mole needs no
// privileges and coexists with local servers owning 0.0.0.0:80 or :443.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/meshdns"
)

const (
	dnsUnitPath = "/etc/systemd/system/mole-dns.service"
	dnsNftPath  = "/etc/mole-dns.nft"
)

// dnsNftRules renders the nft ruleset redirecting virtual-IP traffic on each
// domain port to its shifted listener, and the route IP's port 80 to the Host
// proxy on routePort. It is idempotent: any previous "mole" table is replaced
// atomically.
func dnsNftRules(ports []int, routePort int) string {
	var b strings.Builder
	b.WriteString("table ip mole\ndelete table ip mole\ntable ip mole {\n\tchain out {\n\t\ttype nat hook output priority -100; policy accept;\n")
	fmt.Fprintf(&b, "\t\tip daddr %s tcp dport 80 dnat to 127.0.0.1:%d\n", meshdns.RouteIP, routePort)
	for _, p := range ports {
		fmt.Fprintf(&b, "\t\tip daddr 127.77.0.0/16 tcp dport %d dnat to :%d\n", p, meshdns.BindPort(p))
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}

// dnsStartScript is the shell run at start. systemd skips ExecStop when a
// start fails, so the script rolls back the link it created itself (the nft
// file is applied atomically as the last step: it either exists or doesn't).
func dnsStartScript(routes []string, nftPath string) string {
	return `ip link add mole0 type dummy || exit 1; ` +
		`( ip addr add ` + meshdns.StubIP + `/32 scope global dev mole0` +
		` && ip link set mole0 up` +
		` && resolvectl dns mole0 ` + meshdns.StubAddr +
		` && resolvectl domain mole0 ` + strings.Join(routes, " ") +
		` && resolvectl default-route mole0 no` +
		` && nft -f ` + nftPath + ` ) || { ip link del mole0; exit 1; }`
}

// dnsUnit renders the systemd unit. domains must already be validated: they
// are interpolated into a shell command.
func dnsUnit(domains []string) string {
	routes := make([]string, len(domains))
	for i, d := range domains {
		routes[i] = "~" + d
	}
	return `[Unit]
Description=mole split-DNS link
After=systemd-resolved.service
Wants=systemd-resolved.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c '` + dnsStartScript(routes, dnsNftPath) + `'
ExecStop=/bin/sh -c 'nft delete table ip mole; ip link del mole0'

[Install]
WantedBy=multi-user.target
`
}

func runDNS(args []string) int {
	fs := flag.NewFlagSet("dns", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: sudo mole dns install|uninstall [-config path]

install   route the config's domains: to mole's DNS stub. Linux: a dedicated
          dummy link (mole0) and nftables. macOS: /etc/resolver, an lo0 alias,
          a pf redirect of port 80 and a LaunchDaemon (named routes only).
          Re-run after changing domains:, domain_ports: or route_port:
          (pass -config PATH: sudo may not see your config)
uninstall remove all of it`)
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "install" && fs.Arg(0) != "uninstall") {
		fs.Usage()
		return 2
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "mole dns: only Linux (systemd-resolved) and macOS are supported")
		return 1
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "mole dns: must run as root (sudo mole dns "+fs.Arg(0)+" -config PATH)")
		return 1
	}
	if runtime.GOOS == "darwin" {
		return runDNSMac(fs.Arg(0), *configPath)
	}
	if fs.Arg(0) == "uninstall" {
		_, statErr := os.Stat(dnsUnitPath)
		if statErr == nil {
			if out, err := exec.Command("systemctl", "disable", "--now", "mole-dns.service").CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "mole dns: systemctl disable: %v\n%s", err, out)
				return 1
			}
			if err := os.Remove(dnsUnitPath); err != nil {
				fmt.Fprintln(os.Stderr, "mole dns:", err)
				return 1
			}
			_ = exec.Command("systemctl", "daemon-reload").Run()
		}
		if err := os.Remove(dnsNftPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "mole dns:", err)
			return 1
		}
		if err := removeOwnedLink(); err != nil {
			fmt.Fprintln(os.Stderr, "mole dns:", err)
			return 1
		}
		fmt.Println("removed mole0, the nft table and", dnsUnitPath)
		return 0
	}

	cfg, err := config.Load(resolveConfigPathReadOnly(*configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if len(cfg.Domains) == 0 {
		fmt.Fprintln(os.Stderr, "mole dns: no domains: in the config (e.g. domains: [kena.lan])")
		return 1
	}
	if err := exec.Command("systemctl", "is-active", "--quiet", "systemd-resolved").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "mole dns: systemd-resolved is not active; only it is supported")
		return 1
	}
	if _, err := exec.LookPath("nft"); err != nil {
		fmt.Fprintln(os.Stderr, "mole dns: nft (nftables) not found; install nftables")
		return 1
	}
	if err := os.WriteFile(dnsNftPath, []byte(dnsNftRules(cfg.DomainPortsOrDefault(), cfg.RoutePortOrDefault())), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "mole dns:", err)
		return 1
	}
	if err := os.WriteFile(dnsUnitPath, []byte(dnsUnit(cfg.Domains)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "mole dns:", err)
		return 1
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	// a failed earlier start (or a changed domain list) must not leave a
	// stale mole0 that would make the new start fail with EEXIST
	_ = exec.Command("systemctl", "stop", "mole-dns.service").Run()
	if err := removeOwnedLink(); err != nil {
		fmt.Fprintln(os.Stderr, "mole dns:", err)
		return 1
	}
	if out, err := exec.Command("systemctl", "enable", "mole-dns.service").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "mole dns: systemctl: %v\n%s", err, out)
		return 1
	}
	if out, err := exec.Command("systemctl", "start", "mole-dns.service").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "mole dns: systemctl start: %v\n%s", err, out)
		return 1
	}
	fmt.Printf("routing %s to %s via mole0; ports %v redirected to +%d\n",
		strings.Join(cfg.Domains, ", "), meshdns.StubAddr, cfg.DomainPortsOrDefault(), meshdns.PortShift)
	return 0
}

// removeOwnedLink deletes mole0 only if it carries mole's stub address, so
// an unrelated interface that happens to be named mole0 is never touched,
// and drops mole's own nft table.
func removeOwnedLink() error {
	_ = exec.Command("nft", "delete", "table", "ip", "mole").Run() // absent is fine
	out, err := exec.Command("ip", "-o", "-4", "addr", "show", "dev", "mole0").Output()
	if err != nil || !strings.Contains(string(out), meshdns.StubIP+"/32") {
		return nil // absent or not ours
	}
	if out, err := exec.Command("ip", "link", "del", "mole0").CombinedOutput(); err != nil {
		return fmt.Errorf("remove mole0: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
