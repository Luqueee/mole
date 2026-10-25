package routes

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Luqueee/mole/internal/proxy"
)

func TestLookup(t *testing.T) {
	tb := NewTable([]string{"mole.lan", "Dev.Test."}, map[string]string{"a": "127.0.0.1:3000"})
	for host, want := range map[string]string{
		"a.mole.lan":       "127.0.0.1:3000",
		"A.MOLE.LAN:7080":  "127.0.0.1:3000",
		"a.mole.lan.":      "127.0.0.1:3000",
		"a.dev.test":       "127.0.0.1:3000",
		"b.mole.lan":       "",
		"mole.lan":         "",
		"a.other.lan":      "",
		"x.a.mole.lan":     "", // only one label in front of the domain
		"a":                "",
		"a.mole.lan.extra": "",
	} {
		got, ok := tb.Lookup(host)
		if got != want || ok != (want != "") {
			t.Errorf("Lookup(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
}

func TestTableSetDeleteList(t *testing.T) {
	tb := NewTable([]string{"mole.lan"}, nil)
	tb.Set("b", "127.0.0.1:2")
	tb.Set("a", "127.0.0.1:1")
	l := tb.List()
	if len(l) != 2 || l[0].Name != "a" || l[1].Name != "b" || l[0].Hosts[0] != "a.mole.lan" {
		t.Fatalf("List = %+v", l)
	}
	tb.Delete("a")
	tb.Delete("missing")
	if _, ok := tb.Lookup("a.mole.lan"); ok {
		t.Fatal("deleted route still resolves")
	}
}

// mapDialer plays the SSH tunnel: it records what the proxy asked the remote
// for and connects to the matching local test server instead.
type mapDialer struct {
	mu    sync.Mutex
	to    map[string]string
	asked []string
	nodes []string
}

func (d *mapDialer) DialNode(node, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.nodes = append(d.nodes, node)
	d.mu.Unlock()
	return d.Dial(network, addr)
}

func (d *mapDialer) Dial(network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.asked = append(d.asked, addr)
	real, ok := d.to[addr]
	d.mu.Unlock()
	if !ok {
		return nil, io.ErrClosedPipe
	}
	return net.Dial(network, real)
}

func backend(t *testing.T, name string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, name+" host="+r.Host+" xff="+r.Header.Get("X-Forwarded-For")+" xfh="+r.Header.Get("X-Forwarded-Host"))
	}))
	t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "http://")
}

func get(t *testing.T, h http.Handler, host string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

func TestProxyRoutesByHost(t *testing.T) {
	dial := &mapDialer{to: map[string]string{
		"127.0.0.1:3000": backend(t, "A"),
		"10.0.0.5:8080":  backend(t, "B"),
	}}
	tb := NewTable([]string{"mole.lan"}, map[string]string{"a": "127.0.0.1:3000", "api": "10.0.0.5:8080"})
	p := New(tb, dial, proxy.Hooks{}, slog.New(slog.DiscardHandler))

	if code, body := get(t, p, "a.mole.lan:7080"); code != 200 || !strings.HasPrefix(body, "A host=127.0.0.1:3000 xff=192.0.2.1") || !strings.HasSuffix(body, "xfh=a.mole.lan:7080") {
		t.Errorf("a: %d %q", code, body)
	}
	if code, body := get(t, p, "api.mole.lan"); code != 200 || !strings.HasPrefix(body, "B host=10.0.0.5:8080") || !strings.HasSuffix(body, "xfh=api.mole.lan") {
		t.Errorf("api: %d %q", code, body)
	}
	for _, addr := range dial.asked {
		if addr != "127.0.0.1:3000" && addr != "10.0.0.5:8080" {
			t.Errorf("tunnel dialed %q: only route targets may be dialed", addr)
		}
	}
}

func TestProxyUnknownHostListsRoutes(t *testing.T) {
	tb := NewTable([]string{"mole.lan"}, map[string]string{"a": "127.0.0.1:3000"})
	p := New(tb, &mapDialer{}, proxy.Hooks{}, slog.New(slog.DiscardHandler))
	code, body := get(t, p, "nope.mole.lan")
	if code != http.StatusNotFound || !strings.Contains(body, "a.mole.lan -> 127.0.0.1:3000") {
		t.Errorf("unknown host: %d %q", code, body)
	}
}

func TestProxyRemoteDownIs502(t *testing.T) {
	tb := NewTable([]string{"mole.lan"}, map[string]string{"a": "127.0.0.1:3000"})
	var fails int
	p := New(tb, &mapDialer{}, proxy.Hooks{OnDialFail: func() { fails++ }}, slog.New(slog.DiscardHandler))
	if code, _ := get(t, p, "a.mole.lan"); code != http.StatusBadGateway {
		t.Errorf("code = %d, want 502", code)
	}
	if fails != 1 {
		t.Errorf("OnDialFail called %d times, want 1", fails)
	}
}

func TestProxyReportsConnections(t *testing.T) {
	dial := &mapDialer{to: map[string]string{"127.0.0.1:3000": backend(t, "A")}}
	tb := NewTable([]string{"mole.lan"}, map[string]string{"a": "127.0.0.1:3000"})
	var mu sync.Mutex
	var infos []proxy.ConnInfo
	done := make(chan struct{}, 1)
	hooks := proxy.Hooks{OnConn: func(ci proxy.ConnInfo) {
		mu.Lock()
		infos = append(infos, ci)
		mu.Unlock()
		done <- struct{}{}
	}}
	p := New(tb, dial, hooks, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = "a.mole.lan"
	req.Close = true
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	p.reverseProxy("", false).Transport.(*http.Transport).CloseIdleConnections()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(infos) != 1 || infos[0].Port != 3000 || infos[0].Dir != proxy.DirPull || infos[0].BytesOut == 0 || infos[0].BytesIn == 0 {
		t.Errorf("infos = %+v", infos)
	}
}

// The same address on two nodes is two different services: each request must
// be dialed through the node its route names, and never reuse the other's
// pooled connection.
func TestProxyDialsTheRoutesNode(t *testing.T) {
	dial := &mapDialer{to: map[string]string{"127.0.0.1:3000": backend(t, "X")}}
	tb := NewTable([]string{"mole.lan"}, map[string]string{
		"here":  "127.0.0.1:3000",
		"there": "@dockers/127.0.0.1:3000",
	})
	p := New(tb, dial, proxy.Hooks{}, slog.New(slog.DiscardHandler))

	for _, host := range []string{"here.mole.lan", "there.mole.lan", "here.mole.lan", "there.mole.lan"} {
		if code, _ := get(t, p, host); code != 200 {
			t.Fatalf("%s: %d", host, code)
		}
	}
	dial.mu.Lock()
	defer dial.mu.Unlock()
	counts := map[string]int{}
	for _, n := range dial.nodes {
		counts[n]++
	}
	if counts[""] == 0 || counts["dockers"] == 0 {
		t.Errorf("dials by node = %v, want both the primary and dockers", counts)
	}
}

func TestSplitTarget(t *testing.T) {
	for in, want := range map[string][2]string{
		"127.0.0.1:3000":          {"", "127.0.0.1:3000"},
		"@dockers/127.0.0.1:3000": {"dockers", "127.0.0.1:3000"},
		"@a-b/db:5432":            {"a-b", "db:5432"},
	} {
		if n, a := SplitTarget(in); n != want[0] || a != want[1] {
			t.Errorf("SplitTarget(%q) = %q, %q; want %q, %q", in, n, a, want[0], want[1])
		}
	}
}

func TestSplitSchemeAndHostHeader(t *testing.T) {
	for _, tc := range []struct {
		addr, scheme, hostport, host string
		insecure                     bool
	}{
		{"127.0.0.1:3000", "http", "127.0.0.1:3000", "127.0.0.1:3000", false},
		{"admin.corp:80", "http", "admin.corp:80", "admin.corp", false},
		{"admin.corp:8080", "http", "admin.corp:8080", "admin.corp:8080", false},
		{"https://admin.corp:443", "https", "admin.corp:443", "admin.corp", false},
		{"https://admin.corp:8443", "https", "admin.corp:8443", "admin.corp:8443", false},
		{"https://admin.corp:80", "https", "admin.corp:80", "admin.corp:80", false}, // 80 is not https's default
		{"https+insecure://admin.corp:443", "https", "admin.corp:443", "admin.corp", true},
	} {
		scheme, insecure, hostport := splitScheme(tc.addr)
		if scheme != tc.scheme || hostport != tc.hostport || insecure != tc.insecure {
			t.Errorf("splitScheme(%q) = %q, %v, %q", tc.addr, scheme, insecure, hostport)
		}
		if got := hostHeader(scheme, hostport); got != tc.host {
			t.Errorf("hostHeader(%q, %q) = %q, want %q", scheme, hostport, got, tc.host)
		}
	}
}

// A name-only target sends the name as Host, without the default port.
func TestProxyNameTargetSendsBareHost(t *testing.T) {
	dial := &mapDialer{to: map[string]string{"app.corp:80": backend(t, "N")}}
	tb := NewTable([]string{"mole.lan"}, map[string]string{"app": "app.corp:80"})
	p := New(tb, dial, proxy.Hooks{}, slog.New(slog.DiscardHandler))
	if code, body := get(t, p, "app.mole.lan"); code != 200 || !strings.HasPrefix(body, "N host=app.corp xff=") {
		t.Errorf("%d %q", code, body)
	}
}

// An https target is dialed through the tunnel and TLS is spoken to it with
// the target's name, which is what the certificate is checked against.
func TestProxyHTTPSTarget(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secure host="+r.Host+" tls="+strconv.FormatBool(r.TLS != nil))
	}))
	defer srv.Close()
	dial := &mapDialer{to: map[string]string{"example.com:443": strings.TrimPrefix(srv.URL, "https://")}}
	tb := NewTable([]string{"mole.lan"}, map[string]string{"sec": "https://example.com:443"})
	p := New(tb, dial, proxy.Hooks{}, slog.New(slog.DiscardHandler))
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	p.tls = &tls.Config{RootCAs: pool}

	if code, body := get(t, p, "sec.mole.lan"); code != 200 || body != "secure host=example.com tls=true" {
		t.Errorf("%d %q", code, body)
	}
	// without the right root the handshake must fail, not fall back to plain
	p2 := New(tb, dial, proxy.Hooks{}, slog.New(slog.DiscardHandler))
	if code, _ := get(t, p2, "sec.mole.lan"); code != http.StatusBadGateway {
		t.Errorf("untrusted certificate: %d, want 502", code)
	}
}

// https+insecure:// reaches a service whose certificate nobody trusts, and it
// does not weaken the plain https:// route to the same service.
func TestProxyInsecureTargetSkipsVerificationOnlyForItself(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "internal host="+r.Host)
	}))
	defer srv.Close()
	dial := &mapDialer{to: map[string]string{"internal.corp:443": strings.TrimPrefix(srv.URL, "https://")}}
	tb := NewTable([]string{"mole.lan"}, map[string]string{
		"strict": "https://internal.corp:443",
		"loose":  "https+insecure://internal.corp:443",
	})
	p := New(tb, dial, proxy.Hooks{}, slog.New(slog.DiscardHandler)) // trusts only the system roots

	if code, body := get(t, p, "loose.mole.lan"); code != 200 || body != "internal host=internal.corp" {
		t.Errorf("insecure route: %d %q", code, body)
	}
	// the connection the insecure route left in the pool must not serve this one
	if code, _ := get(t, p, "strict.mole.lan"); code != http.StatusBadGateway {
		t.Errorf("strict route after the insecure one: %d, want 502 (certificate not trusted)", code)
	}
	if code, _ := get(t, p, "loose.mole.lan"); code != 200 {
		t.Errorf("insecure route again: %d", code)
	}
}
