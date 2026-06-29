// Package discover probes a Dialer (typically the SSH tunnel manager)
// for which ports are open on the remote host. Used by the auto-
// discover mode to forward only the ports that actually have something
// listening on the remote.
package discover

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Dialer is the minimal interface needed for discovery: dial a TCP
// address. The tunnel manager satisfies this implicitly.
type Dialer interface {
	Dial(network, addr string) (net.Conn, error)
}

// Runner runs a command on the remote and returns its combined output.
// The tunnel manager satisfies this implicitly.
type Runner interface {
	Run(cmd string) ([]byte, error)
}

// RemoteListeners enumerates TCP ports in LISTEN state on the remote
// that are reachable through the tunnel — i.e. bound to loopback
// (127.0.0.1, ::1) or all interfaces (0.0.0.0, ::). It runs `ss` and
// falls back to `netstat`; returns nil if neither tool is available,
// letting the caller fall back to probing a fixed candidate list.
//
// Unlike the fixed-list Probe, this forwards whatever is actually
// listening, so a server on an unusual port (e.g. 3301) is picked up
// without being pre-registered.
func RemoteListeners(r Runner, log *slog.Logger) []int {
	for _, cmd := range []string{"ss -tlnH", "netstat -tln"} {
		out, err := r.Run(cmd)
		if err != nil {
			log.Debug("listener enumeration failed", "cmd", cmd, "err", err)
			continue
		}
		if ports := parseListeners(string(out)); len(ports) > 0 {
			return ports
		}
	}
	return nil
}

// skipPorts are never auto-forwarded: 22 is the SSH transport itself.
var skipPorts = map[int]bool{22: true}

// parseListeners extracts loopback-reachable LISTEN ports from the
// output of `ss -tlnH` or `netstat -tln`. Both put the local address in
// the 4th whitespace field of every LISTEN line.
func parseListeners(out string) []int {
	seen := make(map[int]bool)
	var ports []int
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "LISTEN") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		host, portStr, ok := splitHostPort(fields[3])
		if !ok || !loopbackReachable(host) {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || skipPorts[port] || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

// splitHostPort splits a "host:port" address from ss/netstat. Unlike
// net.SplitHostPort it tolerates the bracket-less IPv6 form netstat
// prints (e.g. ":::8080" → host "::") by splitting on the last colon.
func splitHostPort(s string) (host, port string, ok bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 || i == len(s)-1 {
		return "", "", false
	}
	host = strings.TrimSuffix(strings.TrimPrefix(s[:i], "["), "]")
	return host, s[i+1:], true
}

// loopbackReachable reports whether a service bound to host is reachable
// via the tunnel, which dials the remote's 127.0.0.1. Anything bound to
// a specific non-loopback address (e.g. a Tailscale or LAN IP) is not.
func loopbackReachable(host string) bool {
	switch host {
	case "0.0.0.0", "::", "127.0.0.1", "::1":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

// Probe returns the subset of candidates that respond to a TCP dial.
// Probes run in parallel; the function blocks until all complete.
// The returned slice is unsorted.
func Probe(d Dialer, candidates []int, log *slog.Logger) []int {
	var (
		mu  sync.Mutex
		out []int
		wg  sync.WaitGroup
	)

	for _, port := range candidates {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			addr := fmt.Sprintf("127.0.0.1:%d", p)
			conn, err := d.Dial("tcp", addr)
			if err != nil {
				return
			}
			_ = conn.Close()
			mu.Lock()
			out = append(out, p)
			mu.Unlock()
			log.Debug("discovered open port", "port", p)
		}(port)
	}
	wg.Wait()
	return out
}
