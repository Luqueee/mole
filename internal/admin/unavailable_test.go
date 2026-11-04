package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const startingReason = "mole is still connecting to dev@box"

// starting is the API a daemon serves before it is connected: no controller
// but routes, which needs no tunnel to be listed.
func starting() *guarded {
	g := &guarded{route: &fakeRouteCtl{routes: map[string]string{"a": "3000"}}}
	g.h = New(NewStats(), map[string]any{"state": "connecting"}).
		WithRouteController(g.route).WithUnavailable(startingReason).Handler()
	return g
}

// Every endpoint that acts through a controller answers 503 with the reason
// while the daemon is starting, whether or not its controller is set.
func TestUnavailableControllerEndpoints(t *testing.T) {
	g := starting()
	for _, c := range []struct{ method, path, body, ctype string }{
		{"POST", "/ports/discover", `{"port":3000}`, ContentTypeJSON},
		{"DELETE", "/ports/discover/3000", "", ""},
		{"POST", "/ports/push", `{"local":3000}`, ContentTypeJSON},
		{"DELETE", "/ports/push/3000", "", ""},
		{"POST", "/routes", `{"name":"b","target":"4000"}`, ContentTypeJSON},
		{"DELETE", "/routes/a", "", ""},
		{"PUT", "/transfer/send?name=a&size=1", "x", ContentTypeFile},
		{"GET", "/transfer/log", "", ""},
		{"GET", "/transfer/pending", "", ""},
		{"POST", "/transfer/pending/1", `{"accept":true}`, ContentTypeJSON},
		{"PUT", "/transfer/receive", `{"on":true}`, ContentTypeJSON},
	} {
		rec := g.serve(c.method, "127.0.0.1:9999", c.path, c.body, map[string]string{"Content-Type": c.ctype})
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), startingReason) {
			t.Errorf("%s %s = %d %q, want 503 with the reason", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if routes := g.route.routes; len(routes) != 1 || routes["a"] != "3000" {
		t.Errorf("a route changed while the daemon was starting: %v", routes)
	}
}

// /status, /health and /events answer while starting; /status carries the
// caller's info (the state) and lists what needs no tunnel.
func TestUnavailableStatusAnswers(t *testing.T) {
	g := starting()
	for _, path := range []string{"/health", "/events", "/status"} {
		if rec := g.serve("GET", "localhost:9999", path, "", nil); rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body)
		}
	}
	rec := g.serve("GET", "localhost:9999", "/status", "", nil)
	var body struct {
		Info struct {
			State  string      `json:"state"`
			Routes []RouteInfo `json:"routes"`
		} `json:"info"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Info.State != "connecting" || len(body.Info.Routes) != 1 {
		t.Errorf("status info = %+v", body.Info)
	}
}

// The browser guard and the Content-Type checks hold in the starting state
// exactly as in the ready one: they are decided before the 503.
func TestUnavailableKeepsTheGuard(t *testing.T) {
	g := starting()
	cases := []struct {
		name, method, host, path, body string
		hdr                            map[string]string
		want                           int
	}{
		{"rebound host", "GET", "attacker.example:9999", "/status", "", nil, http.StatusForbidden},
		{"origin", "POST", "127.0.0.1:9999", "/ports/discover", `{"port":1}`,
			map[string]string{"Content-Type": ContentTypeJSON, "Origin": "https://attacker.example"}, http.StatusForbidden},
		{"cross-site fetch", "GET", "127.0.0.1:9999", "/status", "", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"text/plain body", "POST", "127.0.0.1:9999", "/ports/push", `{"local":1}`,
			map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"form body", "PUT", "127.0.0.1:9999", "/transfer/receive", "on=1",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
	}
	for _, c := range cases {
		if rec := g.serve(c.method, c.host, c.path, c.body, c.hdr); rec.Code != c.want {
			t.Errorf("%s: %s %s = %d, want %d: %s", c.name, c.method, c.path, rec.Code, c.want, rec.Body)
		}
	}
}

// Without WithUnavailable nothing changes: an endpoint whose controller is
// not set is still not mounted.
func TestReadyServerUnchanged(t *testing.T) {
	h := New(NewStats(), nil).Handler()
	req := httptest.NewRequest("POST", "/ports/discover", strings.NewReader(`{"port":1}`))
	req.Host = "127.0.0.1:9999"
	req.Header.Set("Content-Type", ContentTypeJSON)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /ports/discover without a controller = %d, want 404", rec.Code)
	}
}
