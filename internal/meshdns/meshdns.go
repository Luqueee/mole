// Package meshdns implements transparent split-DNS forwarding: a tiny DNS
// stub answers names under the configured domains with a virtual loopback
// IP (127.77.x.y), and every connection to that IP is forwarded through the
// SSH tunnel *by name*, so the remote host does the real DNS resolution.
package meshdns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/Luqueee/mole/internal/proxy"
)

// StubIP is the address mole0 carries and the DNS stub listens on. It must
// have global scope: systemd-resolved ignores links whose only addresses
// are host-scoped (127/8). 198.18.0.0/15 is reserved for benchmarking, so
// it will not collide with a LAN. The port is unprivileged on purpose;
// systemd-resolved accepts addr:port.
const StubIP = "198.18.77.1"

// StubAddr is where the DNS stub listens.
const StubAddr = StubIP + ":5300"

// PortShift is added to every domain port to get the port mole listens on
// for virtual IPs; `mole dns install` redirects port → port+PortShift with
// nftables. Binding the real port would clash with local servers owning
// 0.0.0.0:80/443 and need privileges. Domain ports must be <= 65535-PortShift.
const PortShift = 20000

// BindPort returns the local listening port for domain port p.
func BindPort(p int) int { return p + PortShift }

// maxNames bounds virtual IPs (and listeners) a wildcard domain can create.
// shortcut: flat cap, no eviction; raise or add LRU if real use hits it.
const maxNames = 1024

// Entry is one name → virtual IP mapping.
type Entry struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// Mesh owns the name table and the per-IP forwarding listeners.
type Mesh struct {
	domains []string
	ports   []int
	dialer  proxy.Dialer
	hooks   proxy.Hooks
	log     *slog.Logger

	mu          sync.Mutex
	byName      map[string]net.IP
	order       []string
	lns         []net.Listener
	warnedPorts map[int]bool
	closed      bool
}

// New returns a Mesh forwarding names under domains on ports via dialer.
func New(domains []string, ports []int, dialer proxy.Dialer, hooks proxy.Hooks, log *slog.Logger) *Mesh {
	ds := make([]string, len(domains))
	for i, d := range domains {
		ds[i] = strings.ToLower(strings.Trim(d, "."))
	}
	return &Mesh{
		domains: ds, ports: ports, dialer: dialer, hooks: hooks, log: log,
		byName: map[string]net.IP{}, warnedPorts: map[int]bool{},
	}
}

// Matches reports whether name is inside one of the configured domains.
func (m *Mesh) Matches(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, d := range m.domains {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// Resolve returns the virtual IP for name, allocating it and opening its
// forwarding listeners on first use.
func (m *Mesh) Resolve(name string) (net.IP, error) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("meshdns: closed")
	}
	if ip, ok := m.byName[name]; ok {
		return ip, nil
	}
	if len(m.order) >= maxNames {
		return nil, fmt.Errorf("meshdns: more than %d names", maxNames)
	}
	n := len(m.order) + 2 // .0.1 is the stub
	ip := net.IPv4(127, 77, byte(n>>8), byte(n))
	// All-or-nothing: a name is only published if every port is bound by
	// us. A silently missing port would let that port of the virtual IP
	// fall through to whatever local process owns the wildcard address.
	var opened []net.Listener
	var targets []string
	for _, p := range m.ports {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(BindPort(p))))
		if err != nil {
			for _, l := range opened {
				_ = l.Close()
			}
			if !m.warnedPorts[p] {
				m.warnedPorts[p] = true
				m.log.Warn("could not bind virtual port; not resolving mesh names (port in use locally, or privileged: see `mole dns install`)", "port", p, "err", err)
			}
			return nil, fmt.Errorf("meshdns: bind %s:%d for %s: %w", ip, BindPort(p), name, err)
		}
		opened = append(opened, ln)
		targets = append(targets, net.JoinHostPort(name, strconv.Itoa(p)))
	}
	for i, ln := range opened {
		target, port := targets[i], m.ports[i]
		m.lns = append(m.lns, ln)
		go func() {
			if err := proxy.Serve(ln, m.dialer, target, m.hooks.WithPort(port), m.log); err != nil {
				m.log.Warn("mesh proxy terminated", "target", target, "err", err)
			}
		}()
	}
	m.byName[name] = ip
	m.order = append(m.order, name)
	m.log.Info("mesh name", "name", name, "ip", ip.String(), "ports", m.ports)
	return ip, nil
}

// List returns the current mappings in allocation order.
func (m *Mesh) List() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, 0, len(m.order))
	for _, n := range m.order {
		out = append(out, Entry{Name: n, IP: m.byName[n].String()})
	}
	return out
}

// Close stops every forwarding listener.
func (m *Mesh) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, ln := range m.lns {
		_ = ln.Close()
	}
	m.lns = nil
}

// DNS wire format, only what the stub needs.
const (
	typeA     = 1
	typeAAAA  = 28
	rcodeOK   = 0
	rcodeFail = 2
	rcodeRefu = 5
	rcodeForm = 1
)

// Answer builds the DNS response for query, or nil if it is unusable.
func (m *Mesh) Answer(q []byte) []byte {
	if len(q) < 12 || q[2]&0x80 != 0 { // too short, or already a response
		return nil
	}
	if binary.BigEndian.Uint16(q[4:6]) != 1 {
		return reply(q, 12, rcodeForm, nil)
	}
	name, off, ok := readName(q, 12)
	if !ok || off+4 > len(q) {
		return reply(q, 12, rcodeForm, nil)
	}
	qtype := binary.BigEndian.Uint16(q[off : off+2])
	end := off + 4
	if !m.Matches(name) {
		return reply(q, end, rcodeRefu, nil)
	}
	if qtype != typeA {
		return reply(q, end, rcodeOK, nil) // NODATA: no AAAA/MX/... for mesh names
	}
	ip, err := m.Resolve(name)
	if err != nil {
		m.log.Warn("mesh resolve failed", "name", name, "err", err)
		return reply(q, end, rcodeFail, nil)
	}
	return reply(q, end, rcodeOK, ip.To4())
}

// reply echoes the header and question (q[:end]) with the given rcode and
// an optional A record; EDNS and other additional records are dropped.
func reply(q []byte, end, rcode int, ip4 []byte) []byte {
	out := append([]byte(nil), q[:end]...)
	out[2] = 0x80 | 0x04 | (q[2] & 0x01) // QR, AA, echo RD
	out[3] = 0x80 | byte(rcode)          // RA + rcode
	binary.BigEndian.PutUint16(out[6:], 0)
	binary.BigEndian.PutUint16(out[8:], 0)
	binary.BigEndian.PutUint16(out[10:], 0)
	if ip4 != nil {
		binary.BigEndian.PutUint16(out[6:], 1)
		out = append(out, 0xC0, 0x0C, 0, typeA, 0, 1, 0, 0, 0, 5, 0, 4)
		out = append(out, ip4...)
	}
	return out
}

// readName reads an uncompressed QNAME starting at off.
func readName(b []byte, off int) (string, int, bool) {
	var labels []string
	for {
		if off >= len(b) {
			return "", 0, false
		}
		l := int(b[off])
		off++
		if l == 0 {
			return strings.Join(labels, "."), off, true
		}
		if l&0xC0 != 0 || off+l > len(b) {
			return "", 0, false
		}
		labels = append(labels, string(b[off:off+l]))
		off += l
	}
}

// ServeUDP answers queries on pc until it is closed.
func (m *Mesh) ServeUDP(pc net.PacketConn) error {
	buf := make([]byte, 1500)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if resp := m.Answer(buf[:n]); resp != nil {
			_, _ = pc.WriteTo(resp, addr)
		}
	}
}

// ServeTCP answers length-prefixed queries on ln until it is closed.
func (m *Mesh) ServeTCP(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			defer c.Close()
			var hdr [2]byte
			for {
				if _, err := io.ReadFull(c, hdr[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(hdr[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				resp := m.Answer(q)
				if resp == nil {
					return
				}
				out := binary.BigEndian.AppendUint16(nil, uint16(len(resp)))
				if _, err := c.Write(append(out, resp...)); err != nil {
					return
				}
			}
		}()
	}
}
