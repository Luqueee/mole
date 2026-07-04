package discover

import (
	"reflect"
	"testing"
)

func TestParseListeners_SS(t *testing.T) {
	// Real `ss -tlnH` output (no header), mixed IPv4/IPv6 and bind addrs.
	out := `LISTEN 0      4096                       0.0.0.0:9749       0.0.0.0:*
LISTEN 0      100                      127.0.0.1:25         0.0.0.0:*
LISTEN 0      4096                 100.89.53.125:33562      0.0.0.0:*
LISTEN 0      4096                     127.0.0.1:20241      0.0.0.0:*
LISTEN 0      128                        0.0.0.0:22         0.0.0.0:*
LISTEN 0      4096                       0.0.0.0:3301       0.0.0.0:*
LISTEN 0      4096                          [::]:9749          [::]:*
LISTEN 0      100                          [::1]:25            [::]:*
LISTEN 0      128                           [::]:22            [::]:*
LISTEN 0      4096   [fd7a:115c:a1e0::6938:357e]:62391         [::]:*`

	got := parseListeners(out)
	// 33562 (specific LAN IP) and 62391 (specific IPv6) skipped as not
	// loopback-reachable. Exclusion of reserved ports is the caller's
	// job, so 22 is still returned here. Dedup + sorted.
	want := []int{22, 25, 3301, 9749, 20241}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseListeners = %v, want %v", got, want)
	}
}

func TestParseListeners_Netstat(t *testing.T) {
	// `netstat -tln` includes a header and the state in the last column.
	out := `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State
tcp        0      0 0.0.0.0:3301            0.0.0.0:*               LISTEN
tcp        0      0 127.0.0.1:5432          0.0.0.0:*               LISTEN
tcp6       0      0 :::8080                 :::*                    LISTEN`

	got := parseListeners(out)
	want := []int{3301, 5432, 8080}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseListeners = %v, want %v", got, want)
	}
}

func TestParseListeners_Empty(t *testing.T) {
	if got := parseListeners(""); got != nil {
		t.Errorf("parseListeners(empty) = %v, want nil", got)
	}
}

// TestParseListeners_SS_Wildcard is the regression test for the bug where
// ss's "*" dual-stack shorthand (printed for a service bound to :: with
// v6only=0, e.g. `react-router dev --host` / Vite on 3330) was dropped
// because loopbackReachable did not treat "*" as a wildcard. Port 3330's
// ONLY appearance here is the "*" line, so if "*" were removed from the
// accepted set (the bug) 3330 vanishes and this test reddens. It also
// pins that a specific non-loopback IP (Tailscale 100.x) stays excluded.
func TestParseListeners_SS_Wildcard(t *testing.T) {
	// Real captured `ss -tlnH` output. `*:3330` is the dual-stack Vite /
	// react-router server; `127.0.0.1:20241` a loopback service;
	// `100.89.53.125:33562` a Tailscale-bound service (must be excluded).
	out := `LISTEN 0      511                              *:3330        *:*
LISTEN 0      4096                     127.0.0.1:20241      0.0.0.0:*
LISTEN 0      4096                 100.89.53.125:33562      0.0.0.0:*`

	got := parseListeners(out)
	// 3330 accepted via the "*" wildcard, 20241 via loopback; 33562
	// (specific Tailscale IP) excluded. Sorted ascending.
	want := []int{3330, 20241}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseListeners = %v, want %v", got, want)
	}
	for _, p := range got {
		if p == 33562 {
			t.Errorf("parseListeners returned excluded non-loopback port 33562: %v", got)
		}
	}
}

// TestLoopbackReachable_Wildcard pins the helper contract directly: ss's
// "*" shorthand is loopback-reachable, a specific LAN IP is not. Removing
// "*" from the accepted set (the bug) reddens the first assertion.
func TestLoopbackReachable_Wildcard(t *testing.T) {
	if !loopbackReachable("*") {
		t.Error(`loopbackReachable("*") = false, want true (ss dual-stack shorthand)`)
	}
	if loopbackReachable("192.168.1.60") {
		t.Error(`loopbackReachable("192.168.1.60") = true, want false (specific LAN IP)`)
	}
}
