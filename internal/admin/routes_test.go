package admin

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeRouteCtl struct{ routes map[string]string }

func (f *fakeRouteCtl) SetRoute(name, target string) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrInvalid)
	}
	f.routes[name] = target
	return nil
}

func (f *fakeRouteCtl) DeleteRoute(name string) error { delete(f.routes, name); return nil }

func (f *fakeRouteCtl) ListRoutes() []RouteInfo {
	var out []RouteInfo
	for n, t := range f.routes {
		out = append(out, RouteInfo{Name: n, Hosts: []string{n + ".mole.lan"}, Target: t})
	}
	return out
}

func TestRouteEndpoints(t *testing.T) {
	ctl := &fakeRouteCtl{routes: map[string]string{}}
	srv := httptest.NewServer(New(NewStats(), nil).WithRouteController(ctl).Handler())
	defer srv.Close()

	do := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, _ := do("POST", "/routes", `{"name":"a","target":"3000"}`); code != http.StatusCreated {
		t.Fatalf("set: %d", code)
	}
	if ctl.routes["a"] != "3000" {
		t.Fatalf("routes = %v", ctl.routes)
	}
	if code, _ := do("POST", "/routes", `{"name":"","target":"3000"}`); code != http.StatusBadRequest {
		t.Errorf("invalid name: %d, want 400", code)
	}
	if code, _ := do("POST", "/routes", `nope`); code != http.StatusBadRequest {
		t.Errorf("bad json: %d, want 400", code)
	}
	if _, body := do("GET", "/status", ""); !strings.Contains(body, `"routes":[{"name":"a","hosts":["a.mole.lan"],"target":"3000"}]`) {
		t.Errorf("/status lacks the route: %s", body)
	}
	if code, _ := do("DELETE", "/routes/a", ""); code != http.StatusOK || len(ctl.routes) != 0 {
		t.Errorf("delete: %d %v", code, ctl.routes)
	}
	if code, _ := do("DELETE", "/routes/a", ""); code != http.StatusOK {
		t.Errorf("delete is not idempotent: %d", code)
	}
	if code, _ := do("DELETE", "/routes/", ""); code != http.StatusBadRequest {
		t.Errorf("delete without name: %d, want 400", code)
	}
}
