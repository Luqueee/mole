package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakePushCtl struct{ added []int }

func (f *fakePushCtl) AddPush(local, _ int, _ string) error {
	f.added = append(f.added, local)
	return nil
}
func (f *fakePushCtl) RemovePush(int) error { return nil }
func (f *fakePushCtl) ListPush() []PushInfo { return nil }

// guarded is an admin handler with every controller mounted, and the fakes
// behind it so a test can tell whether a refused request still got through.
type guarded struct {
	h     http.Handler
	ports *fakePortCtl
	push  *fakePushCtl
	route *fakeRouteCtl
	xfer  *fakeTransfer
}

func newGuarded() *guarded {
	g := &guarded{ports: newFakePortCtl(), push: &fakePushCtl{}, route: &fakeRouteCtl{routes: map[string]string{}}, xfer: &fakeTransfer{}}
	g.h = New(NewStats(), nil).WithPortController(g.ports).WithPushController(g.push).
		WithRouteController(g.route).WithTransferController(g.xfer).Handler()
	return g
}

// serve sends one request with the given Host and extra headers ("" values
// are left out) and returns the recorded response.
func (g *guarded) serve(method, host, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	return rec
}

var jsonType = map[string]string{"Content-Type": ContentTypeJSON}

func TestGuardHostHeader(t *testing.T) {
	for _, host := range []string{
		"127.0.0.1:9999", "localhost:9999", "LocalHost:9999", "[::1]:9999", "127.0.0.2:9999",
		"127.0.0.1", "localhost", "[::1]",
	} {
		g := newGuarded()
		if rec := g.serve("GET", host, "/status", "", nil); rec.Code != http.StatusOK {
			t.Errorf("Host %q: GET /status = %d, want 200: %s", host, rec.Code, rec.Body)
		}
		if rec := g.serve("POST", host, "/transfer/pending/7", `{"accept":true}`, jsonType); rec.Code != http.StatusOK || !g.xfer.decided[7] {
			t.Errorf("Host %q: POST /transfer/pending/7 = %d decided=%v, want 200 and accepted", host, rec.Code, g.xfer.decided)
		}
	}

	// DNS rebinding: the page's own name now resolves to 127.0.0.1, but the
	// browser still sends that name as the Host.
	for _, host := range []string{
		"evil.example:9999", "evil.example", "localhost.evil.example:9999", "127.0.0.1.nip.io:9999",
		"0.0.0.0:9999", "192.168.1.20:9999", "[::]:9999", "[fe80::1]:9999", "",
	} {
		g := newGuarded()
		if rec := g.serve("GET", host, "/status", "", nil); rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: GET /status = %d, want 403", host, rec.Code)
		}
		rec := g.serve("POST", host, "/transfer/pending/7", `{"accept":true}`, jsonType)
		if rec.Code != http.StatusForbidden || g.xfer.decided != nil {
			t.Errorf("Host %q: POST /transfer/pending/7 = %d decided=%v, want 403 and untouched", host, rec.Code, g.xfer.decided)
		}
		if host == "evil.example:9999" && !strings.Contains(rec.Body.String(), "loopback") {
			t.Errorf("403 body should say why: %q", rec.Body)
		}
	}
}

func TestGuardRefusesBrowserRequests(t *testing.T) {
	for name, hdr := range map[string]map[string]string{
		"cross-origin":         {"Origin": "https://evil.example"},
		"opaque origin":        {"Origin": "null"},
		"cross-site fetch":     {"Sec-Fetch-Site": "cross-site"},
		"same-site fetch":      {"Sec-Fetch-Site": "same-site"},
		"same-origin fetch":    {"Sec-Fetch-Site": "same-origin"},
		"origin + fetch-site":  {"Origin": "http://localhost:9999", "Sec-Fetch-Site": "same-origin"},
		"loopback page origin": {"Origin": "http://127.0.0.1:3000"},
	} {
		g := newGuarded()
		withJSON := map[string]string{"Content-Type": ContentTypeJSON}
		for k, v := range hdr {
			withJSON[k] = v
		}
		if rec := g.serve("POST", "127.0.0.1:9999", "/ports/push", `{"local":5432}`, withJSON); rec.Code != http.StatusForbidden || len(g.push.added) != 0 {
			t.Errorf("%s: POST /ports/push = %d pushed=%v, want 403 and nothing pushed", name, rec.Code, g.push.added)
		}
		if rec := g.serve("GET", "127.0.0.1:9999", "/transfer/log", "", hdr); rec.Code != http.StatusForbidden {
			t.Errorf("%s: GET /transfer/log = %d, want 403", name, rec.Code)
		}
	}

	// A URL the user typed in the address bar is a navigation, not a page.
	g := newGuarded()
	if rec := g.serve("GET", "127.0.0.1:9999", "/status", "", map[string]string{"Sec-Fetch-Site": "none"}); rec.Code != http.StatusOK {
		t.Errorf("Sec-Fetch-Site none: GET /status = %d, want 200", rec.Code)
	}
}

// A cross-site <form> or fetch() without a preflight can only send these
// bodies; none of them may reach a handler that changes state.
func TestRequireBodyRefusesSimpleRequests(t *testing.T) {
	simple := []string{"text/plain", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""}
	for _, ep := range []struct{ method, path, body string }{
		{"POST", "/ports/discover", `{"port":3330}`},
		{"POST", "/ports/push", `{"local":5432}`},
		{"POST", "/routes", `{"name":"a","target":"3000"}`},
		{"POST", "/transfer/pending/7", `{"accept":true}`},
		{"PUT", "/transfer/receive", `{"on":true}`},
	} {
		for _, ct := range append(simple, "application/jsonx", "application/json;;", "application/octet-stream") {
			g := newGuarded()
			rec := g.serve(ep.method, "127.0.0.1:9999", ep.path, ep.body, map[string]string{"Content-Type": ct})
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Errorf("%s %s with Content-Type %q = %d, want 415", ep.method, ep.path, ct, rec.Code)
			}
			if len(g.ports.active) != 0 || len(g.push.added) != 0 || len(g.route.routes) != 0 || g.xfer.decided != nil || g.xfer.receive {
				t.Errorf("%s %s with Content-Type %q reached the controller", ep.method, ep.path, ct)
			}
			if !strings.Contains(rec.Body.String(), "upgrade the client") || !strings.Contains(rec.Body.String(), ContentTypeJSON) {
				t.Errorf("415 body should name the type and tell old clients to upgrade: %q", rec.Body)
			}
		}
		g := newGuarded()
		rec := g.serve(ep.method, "127.0.0.1:9999", ep.path, ep.body, map[string]string{"Content-Type": "application/json; charset=utf-8"})
		if rec.Code >= 300 {
			t.Errorf("%s %s with application/json; charset=utf-8 = %d: %s", ep.method, ep.path, rec.Code, rec.Body)
		}
	}
}

func TestRequireBodyTransferSend(t *testing.T) {
	for ct, want := range map[string]int{
		ContentTypeFile:                     http.StatusOK,
		"":                                  http.StatusOK, // mole 0.8.4 and older send no Content-Type
		"text/plain":                        http.StatusUnsupportedMediaType,
		"application/x-www-form-urlencoded": http.StatusUnsupportedMediaType,
		"multipart/form-data; boundary=x":   http.StatusUnsupportedMediaType,
		ContentTypeJSON:                     http.StatusUnsupportedMediaType,
	} {
		g := newGuarded()
		rec := g.serve("PUT", "localhost:9999", "/transfer/send?name=a.txt&size=5", "hello", map[string]string{"Content-Type": ct})
		if rec.Code != want {
			t.Errorf("PUT /transfer/send with Content-Type %q = %d, want %d: %s", ct, rec.Code, want, rec.Body)
		}
		if sent := g.xfer.sent.Name != ""; sent != (want == http.StatusOK) {
			t.Errorf("PUT /transfer/send with Content-Type %q: sent=%v", ct, sent)
		}
	}
}

// The requests the mole CLI and the desktop app make all pass.
func TestGuardAllowsLocalClients(t *testing.T) {
	g := newGuarded()
	ts := httptest.NewServer(g.h)
	defer ts.Close()
	port := ts.URL[strings.LastIndex(ts.URL, ":"):]
	for _, host := range []string{"127.0.0.1" + port, "localhost" + port} {
		req, _ := http.NewRequest("POST", "http://"+host+"/ports/discover", strings.NewReader(`{"port":4000}`))
		req.Header.Set("Content-Type", ContentTypeJSON)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
			t.Errorf("%s: POST /ports/discover = %d", host, resp.StatusCode)
		}
	}
	if !g.ports.active[4000] {
		t.Fatal("the port was not added")
	}
}

func TestCheckListenAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9999", "localhost:9999", "LOCALHOST:9999", "[::1]:9999", "127.0.0.5:1"} {
		if err := CheckListenAddr(addr); err != nil {
			t.Errorf("CheckListenAddr(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:9999", ":9999", "[::]:9999", "192.168.1.2:9999", "100.64.0.1:9999", "example.com:9999", "127.0.0.1", "localhost", ""} {
		err := CheckListenAddr(addr)
		if err == nil {
			t.Errorf("CheckListenAddr(%q) = nil, want an error", addr)
			continue
		}
		if !strings.Contains(err.Error(), "127.0.0.1:9999") {
			t.Errorf("CheckListenAddr(%q) = %q, want it to suggest 127.0.0.1:9999", addr, err)
		}
	}
}
