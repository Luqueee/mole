// Package discover probes a Dialer (typically the SSH tunnel manager)
// for which ports are open on the remote host. Used by the auto-
// discover mode to forward only the ports that actually have something
// listening on the remote.
package discover

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
)

// Dialer is the minimal interface needed for discovery: dial a TCP
// address. The tunnel manager satisfies this implicitly.
type Dialer interface {
	Dial(network, addr string) (net.Conn, error)
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