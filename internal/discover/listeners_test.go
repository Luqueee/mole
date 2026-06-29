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
	// 22 skipped (SSH transport); 33562 (specific LAN IP) and 62391
	// (specific IPv6) skipped as not loopback-reachable. Dedup + sorted.
	want := []int{25, 3301, 9749, 20241}
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
