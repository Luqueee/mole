package main

import (
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/Luqueee/mole/internal/config"
)

// The OS is asked for the daemon's ports only when /status does not report
// them at all; an empty list is an answer, not a reason to run lsof.
func TestGetStatusAsksTheOSOnlyWithoutAPortsKey(t *testing.T) {
	for _, tc := range []struct {
		name, info string
		want       []int
		asked      bool
	}{
		{"none forwarded", `{"ports":[]}`, nil, false},
		{"null list", `{"ports":null}`, nil, false},
		{"forwarding", `{"ports":[3000,5173]}`, []int{3000, 5173}, false},
		{"no ports key", `{"remote":"devlabs"}`, []int{4242}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/status" {
					http.NotFound(w, r)
					return
				}
				io.WriteString(w, `{"stats":{"uptime":"1s"},"info":`+tc.info+`}`)
			}))
			asked := false
			orig := osListeningPorts
			osListeningPorts = func(*config.Config) []int { asked = true; return []int{4242} }
			t.Cleanup(func() { osListeningPorts = orig })

			st := NewMoleService().GetStatus()
			if !st.Running || st.Error != "" {
				t.Fatalf("status: %+v", st)
			}
			if asked != tc.asked {
				t.Fatalf("asked the OS = %v, want %v", asked, tc.asked)
			}
			if !reflect.DeepEqual(st.Ports, tc.want) {
				t.Fatalf("ports = %v, want %v", st.Ports, tc.want)
			}
		})
	}
}

// The fallback must not show the daemon's own listeners as forwards.
func TestParseLsofPortsSkipsTheDaemonsOwnListeners(t *testing.T) {
	out := []byte("p321\nf3\nn127.0.0.1:9999\nf4\nn127.0.0.1:7081\nf5\nn127.0.0.1:5300\n" +
		"f6\nn127.0.0.1:9100\nf7\nn127.0.0.1:5173\nf8\nn127.0.0.1:3000\n" +
		"f9\nn[::1]:8080\nf10\nn*:4000\nf11\nn127.77.0.2:20080\n")

	cfg := config.Default()
	cfg.AdminAddr, cfg.RoutePort, cfg.Transfer.Port = "127.0.0.1:9999", 7081, 9100
	if got, want := parseLsofPorts(out, nonForwardPorts(cfg)), []int{3000, 5173}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %v, want %v", got, want)
	}

	// Unset, the route proxy and the receiver use their default ports.
	def := config.Default()
	skip := nonForwardPorts(def)
	for _, p := range []int{config.DefaultRoutePort, config.DefaultTransferPort, 5300} {
		if !skip[p] {
			t.Errorf("port %d is not skipped by default", p)
		}
	}
	if skip[3000] {
		t.Error("a forward port is skipped")
	}
}
