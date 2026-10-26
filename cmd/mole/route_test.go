package main

import (
	"errors"
	"testing"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/routes"
)

func TestRouteManagerSetRoute(t *testing.T) {
	m := &routeManager{
		table:   routes.NewTable([]string{"mole.lan"}, nil),
		hasNode: func(n string) bool { return n == "dockers" },
	}
	for _, tc := range []struct {
		name, target string
		invalid      bool
		want         string
	}{
		{"web", "3000", false, "127.0.0.1:3000"},
		{"db", "@dockers:5432", false, "@dockers/127.0.0.1:5432"},
		{"api", "@dockers/10.0.0.9:8080", false, "@dockers/10.0.0.9:8080"},
		{"ghost", "@nowhere:80", true, ""},
		{"Bad", "3000", true, ""},
		{"web2", "two words", true, ""},
		{"web3", "https://", true, ""},
		{"adm", "https://admin.corp", false, "https://admin.corp:443"},
		{"intra", "intranet", false, "intranet:80"},
	} {
		err := m.SetRoute(tc.name, tc.target)
		if tc.invalid {
			if !errors.Is(err, admin.ErrInvalid) {
				t.Errorf("SetRoute(%q, %q) = %v, want ErrInvalid", tc.name, tc.target, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("SetRoute(%q, %q): %v", tc.name, tc.target, err)
			continue
		}
		if got, _ := m.table.Lookup(tc.name + ".mole.lan"); got != tc.want {
			t.Errorf("route %s = %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, ok := m.table.Lookup("ghost.mole.lan"); ok {
		t.Error("a route to an unknown node was stored")
	}
}

func TestNeedsDNSStub(t *testing.T) {
	for _, tc := range []struct {
		name    string
		domains []string
		want    bool
	}{
		{"none", nil, false},
		{"localhost", []string{"localhost"}, false},
		{"local routes", []string{"dev.localhost", "tools.localhost"}, false},
		{"system DNS", []string{"dev.lan"}, true},
		{"mixed", []string{"dev.localhost", "dev.lan"}, true},
		{"suffix boundary", []string{"notlocalhost", "localhost.example"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsDNSStub(tc.domains); got != tc.want {
				t.Errorf("needsDNSStub(%q) = %v, want %v", tc.domains, got, tc.want)
			}
		})
	}
}
