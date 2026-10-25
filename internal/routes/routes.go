// Package routes serves named routes: an HTTP reverse proxy that picks the
// service on the remote from the Host header, so a.mole.lan and b.mole.lan can
// share one local port while reaching different remote ports.
package routes

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Luqueee/mole/internal/proxy"
)

// Route is one name → remote service mapping, as shown to users.
type Route struct {
	Name   string   `json:"name"`   // label as configured ("a")
	Hosts  []string `json:"hosts"`  // names it answers to ("a.mole.lan")
	Target string   `json:"target"` // host:port on the remote
}

// Table maps names under the configured domains to remote targets. It is safe
// for concurrent use: routes change at runtime (`mole route add`, the app).
type Table struct {
	mu      sync.RWMutex
	domains []string
	routes  map[string]string // label → host:port
}

// NewTable returns a table answering to label.<domain> for every domain.
func NewTable(domains []string, routes map[string]string) *Table {
	t := &Table{routes: make(map[string]string, len(routes))}
	for _, d := range domains {
		t.domains = append(t.domains, strings.ToLower(strings.Trim(d, ".")))
	}
	for k, v := range routes {
		t.routes[k] = v
	}
	return t
}

// Lookup returns the target for a Host header value or DNS name; a port on
// the name is ignored.
func (t *Table) Lookup(host string) (string, bool) {
	host = strings.ToLower(strings.TrimSuffix(stripPort(host), "."))
	label, domain, ok := strings.Cut(host, ".")
	if !ok {
		return "", false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, d := range t.domains {
		if d == domain {
			target, found := t.routes[label]
			return target, found
		}
	}
	return "", false
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// Set adds or replaces a route.
func (t *Table) Set(label, target string) {
	t.mu.Lock()
	t.routes[label] = target
	t.mu.Unlock()
}

// Delete removes a route; removing an unknown one is not an error.
func (t *Table) Delete(label string) {
	t.mu.Lock()
	delete(t.routes, label)
	t.mu.Unlock()
}

// List returns the routes sorted by name.
func (t *Table) List() []Route {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Route, 0, len(t.routes))
	for label, target := range t.routes {
		r := Route{Name: label, Target: target, Hosts: make([]string, 0, len(t.domains))}
		for _, d := range t.domains {
			r.Hosts = append(r.Hosts, label+"."+d)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NodeDialer dials addr on a node's side of the mesh. The empty node is the
// primary remote; any other is a registered node reached over its own tunnel
// (see Pool).
type NodeDialer interface {
	DialNode(node, network, addr string) (net.Conn, error)
}

// SplitTarget splits a canonical target ("host:port" or "@node/host:port")
// into its node ("" for the primary remote) and the address to dial there.
func SplitTarget(target string) (node, addr string) {
	rest, ok := strings.CutPrefix(target, "@")
	if !ok {
		return "", target
	}
	node, addr, _ = strings.Cut(rest, "/")
	return node, addr
}

// splitScheme splits the address of a canonical target into its scheme
// ("http" unless it starts with "https://" or "https+insecure://"), whether the
// certificate is left unchecked, and host:port.
func splitScheme(addr string) (scheme string, insecure bool, hostport string) {
	if rest, ok := strings.CutPrefix(addr, "https+insecure://"); ok {
		return "https", true, rest
	}
	if rest, ok := strings.CutPrefix(addr, "https://"); ok {
		return "https", false, rest
	}
	return "http", false, addr
}

// hostHeader is the Host the service is sent: the target's name, without the
// port when it is the scheme's default (virtual hosts match "admin.corp", not
// "admin.corp:80").
func hostHeader(scheme, hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err == nil && ((scheme == "http" && port == "80") || (scheme == "https" && port == "443")) {
		return host
	}
	return hostport
}

// upstream is where one request goes, carried in its context.
type upstream struct {
	scheme, hostport string
	insecure         bool // https without checking the certificate
}

// Proxy forwards HTTP requests to the target their Host header names, through
// the tunnel of the node that target lives on.
type Proxy struct {
	table *Table
	dial  NodeDialer
	hooks proxy.Hooks
	log   *slog.Logger
	// tls configures the TLS spoken to https targets (nil: system roots).
	tls *tls.Config

	mu  sync.Mutex
	rps map[rpKey]*httputil.ReverseProxy
}

type ctxKey struct{}

// New returns a Proxy for table. Connections show up in the stats like any
// other pull, under the target's port.
func New(table *Table, dial NodeDialer, hooks proxy.Hooks, log *slog.Logger) *Proxy {
	return &Proxy{table: table, dial: dial, hooks: hooks, log: log, rps: map[rpKey]*httputil.ReverseProxy{}}
}

// rpKey tells proxies apart: each node and each way of checking certificates
// gets its own Transport, since idle connections are pooled by address:
// "127.0.0.1:3000" on two nodes must not share them, and a connection made
// without checking a certificate must never serve a request that wanted it
// checked.
type rpKey struct {
	node     string
	insecure bool
}

// reverseProxy returns the proxy for one node.
func (p *Proxy) reverseProxy(node string, insecure bool) *httputil.ReverseProxy {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := rpKey{node, insecure}
	if rp, ok := p.rps[key]; ok {
		return rp
	}
	tlsCfg := p.tls
	if insecure {
		tlsCfg = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the route asked for it: https+insecure://
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			up, _ := pr.In.Context().Value(ctxKey{}).(upstream)
			pr.Out.URL.Scheme = up.scheme
			pr.Out.URL.Host = up.hostport
			// The service sees its own address as Host: dev servers (Vite's
			// allowedHosts, Ollama, Django's ALLOWED_HOSTS) refuse a name
			// they do not know. The name the user typed travels in
			// X-Forwarded-Host.
			pr.Out.Host = hostHeader(up.scheme, up.hostport)
			pr.SetXForwarded()
		},
		Transport: &http.Transport{
			// The address is the one Rewrite chose; the tunnel dials it on
			// the node, so nothing is resolved locally.
			DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialTracked(p.dial, node, network, addr, p.hooks)
			},
			// For https targets the Transport wraps the tunnel connection in
			// TLS itself, with the target's name as SNI and for verification.
			TLSClientConfig:     tlsCfg,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     30 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			where := "the remote"
			if node != "" {
				where = "node " + node
			}
			p.log.Warn("route request failed", "host", r.Host, "node", node, "err", err)
			http.Error(w, fmt.Sprintf("mole: %s is routed but %s did not answer: %v", r.Host, where, err), http.StatusBadGateway)
		},
	}
	p.rps[key] = rp
	return rp
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, ok := p.table.Lookup(r.Host)
	if !ok {
		var b strings.Builder
		fmt.Fprintf(&b, "mole: no route for %q\n", r.Host)
		for _, rt := range p.table.List() {
			fmt.Fprintf(&b, "  %s -> %s\n", strings.Join(rt.Hosts, ", "), rt.Target)
		}
		http.Error(w, b.String(), http.StatusNotFound)
		return
	}
	node, addr := SplitTarget(target)
	scheme, insecure, hostport := splitScheme(addr)
	p.reverseProxy(node, insecure).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, upstream{scheme, hostport, insecure})))
}

// Serve answers requests on ln until it is closed.
func (p *Proxy) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 15 * time.Second}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed && !isClosed(err) {
		return err
	}
	return nil
}

func isClosed(err error) bool {
	return strings.Contains(err.Error(), "use of closed network connection")
}

// dialTracked dials addr and reports the connection to hooks the way
// proxy.Serve does for plain forwards.
func dialTracked(dial NodeDialer, node, network, addr string, hooks proxy.Hooks) (net.Conn, error) {
	port, _ := strconv.Atoi(portOf(addr))
	hooks = hooks.WithPort(port)
	start := time.Now()
	c, err := dial.DialNode(node, network, addr)
	if err != nil {
		if hooks.OnDialFail != nil {
			hooks.OnDialFail()
		}
		if hooks.OnConn != nil {
			hooks.OnConn(proxy.ConnInfo{Dir: proxy.DirPull, Start: start, Duration: time.Since(start), DialFail: true})
		}
		return nil, err
	}
	if hooks.OnConnect != nil {
		hooks.OnConnect()
	}
	return &trackedConn{Conn: c, hooks: hooks, start: start}, nil
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

type trackedConn struct {
	net.Conn
	hooks     proxy.Hooks
	start     time.Time
	in, out   atomic.Int64
	closeOnce sync.Once
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.in.Add(int64(n))
	return n, err
}

func (c *trackedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.out.Add(int64(n))
	return n, err
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() {
		if c.hooks.OnDisconnect != nil {
			c.hooks.OnDisconnect()
		}
		if c.hooks.OnConn != nil {
			c.hooks.OnConn(proxy.ConnInfo{
				Dir: proxy.DirPull, Start: c.start, Duration: time.Since(c.start),
				BytesOut: c.out.Load(), BytesIn: c.in.Load(),
			})
		}
	})
	return err
}
