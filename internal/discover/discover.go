// Package discover probes a Dialer (typically the SSH tunnel manager)
// for which ports are open on the remote host. Used by the auto-
// discover mode to forward only the ports that actually have something
// listening on the remote.
package discover

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dialer is the minimal interface needed for discovery: dial a TCP
// address with cancellation. The per-sweep dialer from
// tunnel.Manager.NewProbeDialer satisfies it.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// SweepDialer is a Dialer whose lifetime can be scoped to one discovery
// sweep. Implementations should close any transport that backs the dialer.
type SweepDialer interface {
	Dialer
	Close() error
}

// Runner runs a command on the remote and returns its combined output.
// The tunnel manager satisfies this implicitly.
type Runner interface {
	Run(cmd string) ([]byte, error)
}

// listenerCommand is one way of asking the remote for its TCP listeners.
type listenerCommand struct {
	cmd string
	// emptyOK marks a command whose clean, empty output means "nothing is
	// listening". `ss -H` prints no header, so an idle host prints nothing;
	// every netstat prints a header even then, so for netstat empty output
	// proves nothing.
	emptyOK bool
}

// listenerCommands are tried in order until one yields listeners:
//
//   - `ss -tlnH`: Linux (iproute2).
//   - `netstat -an -p tcp`: macOS and the BSDs (BSD table, "host.port"
//     addresses), and Windows OpenSSH remotes whose shell is cmd.exe or
//     PowerShell (Windows table, IPv4 only). macOS needs -a: without it
//     netstat lists no listening sockets at all.
//   - `netstat -tln`: Linux net-tools.
//
// Every output is validated by parseListeners, so a command that runs but
// means something else on a given OS (macOS also accepts `netstat -tln`
// and prints connections only) cannot pass for an empty listener table.
var listenerCommands = []listenerCommand{
	{cmd: "ss -tlnH", emptyOK: true},
	{cmd: "netstat -an -p tcp"},
	{cmd: "netstat -tln"},
}

// RemoteListeners enumerates TCP ports in LISTEN state on the remote
// that are reachable through the tunnel — i.e. bound to loopback
// (127.0.0.1, ::1) or all interfaces (0.0.0.0, ::, or the "*" wildcard).
// It runs `ss`, then falls back to `netstat` (see listenerCommands), and
// returns the first non-empty result.
//
// The bool return reports whether enumeration was authoritative: true if
// at least one command ran successfully AND printed a listener table we
// could read — even when it found zero ports, meaning the remote
// genuinely has nothing listening — and false if no tool was available,
// none printed a table we recognise, or the transport was down. Callers
// use it to tell "prune everything" (authoritative empty) apart from
// "can't tell, fall back to probing the candidate list" (enumeration
// failed). Output we cannot read is never authoritative.
//
// Unlike the fixed-list Probe, this forwards whatever is actually
// listening, so a server on an unusual port (e.g. 3301) is picked up
// without being pre-registered.
func RemoteListeners(r Runner, log *slog.Logger) ([]int, bool) {
	authoritative := false
	for _, c := range listenerCommands {
		out, err := r.Run(c.cmd)
		if err != nil {
			log.Debug("listener enumeration failed", "cmd", c.cmd, "err", err)
			continue
		}
		ports, ok := parseListeners(string(out))
		if !ok && !(c.emptyOK && strings.TrimSpace(string(out)) == "") {
			log.Debug("listener enumeration output not recognised", "cmd", c.cmd, "output", clip(out, 200))
			continue
		}
		authoritative = true
		if len(ports) > 0 {
			return ports, true
		}
	}
	return nil, authoritative
}

// clip shortens command output for a log line.
func clip(out []byte, n int) string {
	s := strings.TrimSpace(string(out))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// parseListeners extracts loopback-reachable LISTEN ports from the
// output of one of listenerCommands. It reads three layouts:
//
//	ss:              LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:*
//	netstat (Unix):  tcp4 0 0 127.0.0.1.5432 *.* LISTEN     (BSD "host.port")
//	                 tcp  0 0 127.0.0.1:5432 0.0.0.0:* LISTEN (Linux)
//	netstat (Win):   TCP 127.0.0.1:5432 0.0.0.0:0 LISTENING
//
// In netstat the local address is two fields before the state. Rows in any
// other state (ESTABLISHED, TIME_WAIT…), UDP rows and banners are ignored.
//
// ok reports whether out was recognisably a listener table: every LISTEN
// row parsed, and there was at least one, or a header saying the table
// includes servers ("only servers", "including servers", "servers and
// established") vouches that there were none. A LISTEN row we cannot read
// makes the whole output untrusted, so a format we do not understand can
// never be mistaken for "nothing is listening". Filtering of
// excluded/reserved ports is the caller's job.
func parseListeners(out string) (ports []int, ok bool) {
	seen := make(map[int]bool)
	parsed, failed := 0, 0
	serversHeader := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "Active Internet connections") && strings.Contains(line, "servers") {
			serversHeader = true
			continue
		}
		fields := strings.Fields(line)
		state := -1
		for i, f := range fields {
			if f == "LISTEN" || f == "LISTENING" {
				state = i
				break
			}
		}
		if state < 0 {
			continue
		}
		var local string
		switch {
		case state == 0 && len(fields) >= 4: // ss
			local = fields[3]
		case state >= 2 && strings.EqualFold(fields[0], "unix"):
			continue // Linux `netstat -a` lists Unix sockets as LISTENING
		case state >= 2 && strings.HasPrefix(strings.ToLower(fields[0]), "tcp"): // netstat
			local = fields[state-2]
		default:
			failed++
			continue
		}
		host, port, ok := splitHostPort(local)
		if !ok {
			failed++
			continue
		}
		parsed++
		if !loopbackReachable(host) || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports, failed == 0 && (parsed > 0 || serversHeader)
}

// splitHostPort splits a listener's local address into host and port. It
// reads the colon form of ss and Linux/Windows netstat ("127.0.0.1:5432",
// "[::1]:25", "*:3330", and the bracket-less IPv6 ":::8080", split on the
// last colon) and the BSD form, which puts a '.' before the port
// ("127.0.0.1.3774", "*.3000", "::1.8080", "fe80::1%lo0.8080"). The two
// cannot be confused: in the BSD form the text after the last colon always
// contains the '.', so it is never a bare port number.
func splitHostPort(s string) (host string, port int, ok bool) {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		if p, ok := parsePort(s[i+1:]); ok {
			return strings.TrimSuffix(strings.TrimPrefix(s[:i], "["), "]"), p, true
		}
	}
	if i := strings.LastIndex(s, "."); i > 0 {
		if p, ok := parsePort(s[i+1:]); ok {
			return s[:i], p, true
		}
	}
	return "", 0, false
}

// parsePort parses a decimal TCP port, 1-65535.
func parsePort(s string) (int, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	p, err := strconv.Atoi(s)
	return p, err == nil && p >= 1 && p <= 65535
}

// loopbackReachable reports whether a service bound to host is reachable
// via the tunnel, which dials the remote's 127.0.0.1. The unspecified
// address — 0.0.0.0, ::, or the "*" shorthand ss prints for a dual-stack
// bind and BSD netstat prints for any wildcard bind — and loopback
// (127.0.0.0/8, ::1) qualify; a specific non-loopback address (e.g. a
// Tailscale or LAN IP, or a link-local fe80::1%lo0) does not.
func loopbackReachable(host string) bool {
	switch host {
	case "*", "0.0.0.0", "::", "127.0.0.1", "::1":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

const defaultProbeTimeout = 5 * time.Second

const maxProbeConcurrency = 4

// Probe returns the subset of candidates that respond to a TCP dial.
// Probes run in parallel; the function blocks until all complete or ctx is
// cancelled. Each probe has its own bounded deadline. The returned slice is
// unsorted.
func Probe(ctx context.Context, d Dialer, candidates []int, log *slog.Logger) []int {
	return probeWithTimeout(ctx, d, candidates, log, defaultProbeTimeout)
}

// ProbeWithFactory runs one bounded discovery sweep using a single temporary
// dialer. The factory is called once, so SSH-backed implementations perform
// one transport handshake per sweep rather than one handshake per candidate.
// The sweep timeout also bounds all in-flight channel opens; closing the
// dialer after the workers finish guarantees that a cancelled SSH channel
// cannot outlive the transport that owns it.
func ProbeWithFactory(ctx context.Context, factory func(context.Context) (SweepDialer, error), candidates []int, log *slog.Logger) ([]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sweepCtx, cancel := context.WithTimeout(ctx, defaultProbeTimeout)
	defer cancel()
	d, err := factory(sweepCtx)
	if err != nil {
		log.Debug("probe transport setup failed", "err", err)
		return nil, fmt.Errorf("probe transport setup: %w", err)
	}
	if d == nil {
		err := errors.New("probe factory returned a nil dialer")
		log.Debug("probe transport setup failed", "err", err)
		return nil, err
	}
	defer d.Close()
	return probeWithSweep(sweepCtx, d, candidates, log), nil
}

func probeWithSweep(ctx context.Context, d Dialer, candidates []int, log *slog.Logger) []int {
	workerCount := min(maxProbeConcurrency, len(candidates))
	if workerCount == 0 {
		return nil
	}

	var (
		mu   sync.Mutex
		out  []int
		wg   sync.WaitGroup
		jobs = make(chan int)
	)
	worker := func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case port, ok := <-jobs:
				if !ok {
					return
				}
				addr := fmt.Sprintf("127.0.0.1:%d", port)
				conn, err := d.DialContext(ctx, "tcp", addr)
				if err != nil {
					continue
				}
				if ctx.Err() != nil {
					_ = conn.Close()
					return
				}
				_ = conn.Close()
				mu.Lock()
				out = append(out, port)
				mu.Unlock()
				log.Debug("discovered open port", "port", port)
			}
		}
	}

	wg.Add(workerCount)
	for range workerCount {
		go worker()
	}

send:
	for _, port := range candidates {
		select {
		case <-ctx.Done():
			break send
		case jobs <- port:
		}
	}
	close(jobs)
	wg.Wait()
	sort.Ints(out)
	return out
}

func probeWithTimeout(ctx context.Context, d Dialer, candidates []int, log *slog.Logger, timeout time.Duration) []int {
	if ctx == nil {
		ctx = context.Background()
	}
	var (
		mu  sync.Mutex
		out []int
		wg  sync.WaitGroup
	)

	for _, port := range candidates {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			addr := fmt.Sprintf("127.0.0.1:%d", p)
			conn, err := d.DialContext(probeCtx, "tcp", addr)
			if err != nil {
				return
			}
			if probeCtx.Err() != nil {
				_ = conn.Close()
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
