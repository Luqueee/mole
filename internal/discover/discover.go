// Package discover probes a Dialer (typically the SSH tunnel manager)
// for which ports are open on the remote host. Used by the auto-
// discover mode to forward only the ports that actually have something
// listening on the remote.
package discover

import (
	"bufio"
	"context"
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
// address with cancellation. The tunnel manager satisfies this implicitly.
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

// RemoteListeners enumerates TCP ports in LISTEN state on the remote
// that are reachable through the tunnel — i.e. bound to loopback
// (127.0.0.1, ::1) or all interfaces (0.0.0.0, ::, or ss's "*"). It runs
// falls back to `netstat`.
//
// The bool return reports whether enumeration was authoritative: true if
// at least one of ss/netstat ran successfully — even when it found zero
// ports, meaning the remote genuinely has nothing listening — and false
// if neither tool was available or the transport was down. Callers use
// it to tell "prune everything" (authoritative empty) apart from "can't
// tell, fall back to probing the candidate list" (enumeration failed).
//
// Unlike the fixed-list Probe, this forwards whatever is actually
// listening, so a server on an unusual port (e.g. 3301) is picked up
// without being pre-registered.
func RemoteListeners(r Runner, log *slog.Logger) ([]int, bool) {
	ok := false
	for _, cmd := range []string{"ss -tlnH", "netstat -tln"} {
		out, err := r.Run(cmd)
		if err != nil {
			log.Debug("listener enumeration failed", "cmd", cmd, "err", err)
			continue
		}
		ok = true
		if ports := parseListeners(string(out)); len(ports) > 0 {
			return ports, true
		}
	}
	return nil, ok
}

// parseListeners extracts loopback-reachable LISTEN ports from the
// output of `ss -tlnH` or `netstat -tln`. Both put the local address in
// the 4th whitespace field of every LISTEN line. Filtering of
// excluded/reserved ports is the caller's job.
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
		if err != nil || seen[port] {
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
// via the tunnel, which dials the remote's 127.0.0.1. The unspecified
// address — 0.0.0.0, ::, or the "*" shorthand ss prints for a dual-stack
// bind — and loopback (127.0.0.0/8, ::1) qualify; a specific non-loopback
// address (e.g. a Tailscale or LAN IP) does not.
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
func ProbeWithFactory(ctx context.Context, factory func(context.Context) (SweepDialer, error), candidates []int, log *slog.Logger) []int {
	if ctx == nil {
		ctx = context.Background()
	}
	sweepCtx, cancel := context.WithTimeout(ctx, defaultProbeTimeout)
	defer cancel()
	d, err := factory(sweepCtx)
	if err != nil {
		log.Debug("probe transport setup failed", "err", err)
		return nil
	}
	defer d.Close()
	return probeWithSweep(sweepCtx, d, candidates, log)
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
