package discover

import (
	"reflect"
	"testing"
)

// Fixtures below are captured output with addresses swapped for the
// documentation ranges (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24).

// Linux `ss -tlnH` (no header), mixed IPv4/IPv6 and bind addrs.
const fixtureLinuxSS = `LISTEN 0      4096                       0.0.0.0:9749       0.0.0.0:*
LISTEN 0      100                      127.0.0.1:25         0.0.0.0:*
LISTEN 0      4096                 100.89.53.125:33562      0.0.0.0:*
LISTEN 0      4096                     127.0.0.1:20241      0.0.0.0:*
LISTEN 0      128                        0.0.0.0:22         0.0.0.0:*
LISTEN 0      4096                       0.0.0.0:3301       0.0.0.0:*
LISTEN 0      4096                   127.0.0.53%lo:53         0.0.0.0:*
LISTEN 0      4096                          [::]:9749          [::]:*
LISTEN 0      100                          [::1]:25            [::]:*
LISTEN 0      128                           [::]:22            [::]:*
LISTEN 0      4096   [fd7a:115c:a1e0::6938:357e]:62391         [::]:*`

// Linux net-tools `netstat -tln`: header, state in the last column.
const fixtureLinuxNetstat = `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State
tcp        0      0 0.0.0.0:3301            0.0.0.0:*               LISTEN
tcp        0      0 127.0.0.1:5432          0.0.0.0:*               LISTEN
tcp        0      0 192.0.2.7:9100          0.0.0.0:*               LISTEN
tcp6       0      0 :::8080                 :::*                    LISTEN
tcp6       0      0 fe80::1%eth0:8443       :::*                    LISTEN`

// Linux `netstat -tln` on a host with nothing listening: the header alone,
// which says the table holds servers.
const fixtureLinuxNetstatNone = `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State`

// Linux `netstat -an`: TCP rows plus a Unix-socket section whose rows are
// also LISTENING but are not TCP.
const fixtureLinuxNetstatAll = `Active Internet connections (servers and established)
Proto Recv-Q Send-Q Local Address           Foreign Address         State
tcp        0      0 127.0.0.1:5432          0.0.0.0:*               LISTEN
tcp        0     36 192.0.2.7:22            198.51.100.4:51234      ESTABLISHED
tcp6       0      0 :::3000                 :::*                    LISTEN
udp        0      0 127.0.0.53:53           0.0.0.0:*
Active UNIX domain sockets (servers and established)
Proto RefCnt Flags       Type       State         I-Node   Path
unix  2      [ ACC ]     STREAM     LISTENING     21334    /run/systemd/private
unix  3      [ ]         STREAM     CONNECTED     22101`

// macOS `netstat -tln`: exits 0 and prints the BSD table, but without -a
// it lists no listening sockets — only connections. This must never be
// read as "nothing is listening".
const fixtureMacNetstatTLN = `Active Internet connections
Proto Recv-Q Send-Q  Local Address                                 Foreign Address                               (state)
tcp4       0      0  192.0.2.29.53472                              203.0.113.165.443                             ESTABLISHED
tcp6       0      0  fe80::88b:8cc8:3b82:221c%en0.53469            fe80::c54:686a:446:18e8%en0.49758             ESTABLISHED
tcp4       0      0  127.0.0.1.63356                               127.0.0.1.53440                               ESTABLISHED
tcp4       0      0  192.0.2.29.53334                              198.51.100.210.443                            TIME_WAIT
udp4       0      0  *.5353                                        *.*
Active Multipath Internet connections
Proto/ID  Flags      Local Address          Foreign Address        (state)
Active LOCAL (UNIX) domain sockets
Address          Type   Recv-Q Send-Q            Inode             Conn             Refs          Nextref Addr
df6b76e8c5299111 stream      0      0                0 9cb689f0d1e6f81a                0                0 /var/run/com.apple.launchd.XiSZXAbi3X/Listeners`

// macOS `netstat -an -p tcp`: BSD "host.port" addresses, LISTEN mixed with
// connections; tcp46 is a dual-stack wildcard; long IPv6 addresses are
// truncated but keep their ".port".
const fixtureMacNetstatAN = `Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp6       0    380  fe80::88b:8cc8:3.53633 fe80::c54:686a:4.62078 ESTABLISHED
tcp4       0      0  192.0.2.29.54757       192.0.2.1.49152        SYN_SENT
tcp4       0      0  127.0.0.1.3774         *.*                    LISTEN
tcp46      0      0  *.3000                 *.*                    LISTEN
tcp6       0      0  ::1.8080               *.*                    LISTEN
tcp6       0      0  fe80::1%lo0.8081       *.*                    LISTEN
tcp4       0      0  192.0.2.29.5000        *.*                    LISTEN
tcp4       0      0  *.57621                *.*                    LISTEN
tcp6       0      0  *.57621                *.*                    LISTEN
tcp4       0      0  192.0.2.29.53334       198.51.100.210.443     TIME_WAIT  `

// macOS `netstat -an -p tcp` with nothing listening: the header says the
// table includes servers, and only connections follow.
const fixtureMacNetstatANNone = `Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp4       0      0  192.0.2.29.53472       203.0.113.165.443      ESTABLISHED`

// FreeBSD `netstat -an -p tcp`.
const fixtureFreeBSDNetstatAN = `Active Internet connections (including servers)
Proto Recv-Q Send-Q Local Address          Foreign Address        (state)
tcp4       0     44 192.0.2.40.22          198.51.100.4.60311     ESTABLISHED
tcp4       0      0 127.0.0.1.5432         *.*                    LISTEN
tcp6       0      0 ::1.5432               *.*                    LISTEN
tcp4       0      0 *.22                   *.*                    LISTEN
tcp6       0      0 *.22                   *.*                    LISTEN
tcp4       0      0 192.0.2.40.9100        *.*                    LISTEN`

// Windows `netstat -an -p tcp` (cmd.exe or PowerShell over OpenSSH): CRLF
// line endings, the local address in the 2nd field, state LISTENING.
const fixtureWindowsNetstat = "\r\n" +
	"Active Connections\r\n" +
	"\r\n" +
	"  Proto  Local Address          Foreign Address        State\r\n" +
	"  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING\r\n" +
	"  TCP    0.0.0.0:5040           0.0.0.0:0              LISTENING\r\n" +
	"  TCP    127.0.0.1:3000         0.0.0.0:0              LISTENING\r\n" +
	"  TCP    192.0.2.50:139         0.0.0.0:0              LISTENING\r\n" +
	"  TCP    127.0.0.1:3000         127.0.0.1:52144        ESTABLISHED\r\n" +
	"  TCP    192.0.2.50:22          198.51.100.4:61022     ESTABLISHED\r\n"

// Windows TCPv6 rows, as `netstat -an` prints them.
const fixtureWindowsNetstatV6 = "\r\n" +
	"Active Connections\r\n" +
	"\r\n" +
	"  Proto  Local Address          Foreign Address        State\r\n" +
	"  TCP    [::]:445               [::]:0                 LISTENING\r\n" +
	"  TCP    [::1]:5173             [::]:0                 LISTENING\r\n" +
	"  TCP    [fe80::1%7]:8080       [::]:0                 LISTENING\r\n" +
	"  UDP    [::]:500               *:*\r\n"

// Windows netstat with an unsupported flag (`netstat -tln`) prints its usage
// and may exit 0; nothing in it is a table.
const fixtureWindowsUsage = "\r\n" +
	"Displays protocol statistics and current TCP/IP network connections.\r\n" +
	"\r\n" +
	"NETSTAT [-a] [-b] [-e] [-f] [-i] [-n] [-o] [-p proto] [-r] [-s] [-t] [-x] [-y] [interval]\r\n" +
	"\r\n" +
	"  -a            Displays all connections and listening ports.\r\n"

// A localised Windows netstat: neither the header nor the state is in
// English, so no row can be read.
const fixtureWindowsLocalised = "\r\n" +
	"Conexiones activas\r\n" +
	"\r\n" +
	"  Proto  Dirección local          Dirección remota        Estado\r\n" +
	"  TCP    0.0.0.0:135            0.0.0.0:0              ESCUCHANDO\r\n" +
	"  TCP    127.0.0.1:3000         0.0.0.0:0              ESCUCHANDO\r\n"

func TestParseListeners(t *testing.T) {
	tests := []struct {
		name   string
		out    string
		want   []int
		wantOK bool
	}{
		{
			// 33562 (Tailscale IP) and 62391 (specific IPv6) are not
			// loopback-reachable. Reserved-port exclusion is the caller's
			// job, so 22 is still returned. Deduplicated and sorted.
			name: "linux ss", out: fixtureLinuxSS,
			want: []int{22, 25, 53, 3301, 9749, 20241}, wantOK: true,
		},
		{
			// 9100 is bound to a LAN IP, 8443 to a link-local address.
			name: "linux netstat -tln", out: fixtureLinuxNetstat,
			want: []int{3301, 5432, 8080}, wantOK: true,
		},
		{
			name: "linux netstat -tln, nothing listening", out: fixtureLinuxNetstatNone,
			want: nil, wantOK: true,
		},
		{
			name: "linux netstat -an skips unix sockets", out: fixtureLinuxNetstatAll,
			want: []int{3000, 5432}, wantOK: true,
		},
		{
			// The confirmed bug: this used to be an authoritative empty
			// result, which pruned every auto-discovered forward.
			name: "macOS netstat -tln lists no servers", out: fixtureMacNetstatTLN,
			want: nil, wantOK: false,
		},
		{
			// 8081 is bound to the link-local fe80::1%lo0, 5000 to a LAN IP.
			name: "macOS netstat -an -p tcp", out: fixtureMacNetstatAN,
			want: []int{3000, 3774, 8080, 57621}, wantOK: true,
		},
		{
			name: "macOS netstat -an -p tcp, nothing listening", out: fixtureMacNetstatANNone,
			want: nil, wantOK: true,
		},
		{
			name: "FreeBSD netstat -an -p tcp", out: fixtureFreeBSDNetstatAN,
			want: []int{22, 5432}, wantOK: true,
		},
		{
			// 139 is bound to a LAN IP; ESTABLISHED rows are ignored.
			name: "windows netstat -an -p tcp", out: fixtureWindowsNetstat,
			want: []int{135, 3000, 5040}, wantOK: true,
		},
		{
			name: "windows netstat IPv6 rows", out: fixtureWindowsNetstatV6,
			want: []int{445, 5173}, wantOK: true,
		},
		{
			name: "windows netstat usage text", out: fixtureWindowsUsage,
			want: nil, wantOK: false,
		},
		{
			name: "localised windows netstat", out: fixtureWindowsLocalised,
			want: nil, wantOK: false,
		},
		{
			name: "shell error", out: "sh: 1: netstat: not found\n",
			want: nil, wantOK: false,
		},
		{
			name: "garbage mentioning LISTEN", out: "error: LISTEN is not supported here\n",
			want: nil, wantOK: false,
		},
		{
			// One LISTEN row we cannot read taints the whole output, even
			// though the other rows parse.
			name: "a LISTEN row that does not parse",
			out: "LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:*\n" +
				"LISTEN 0 4096 somewhere 0.0.0.0:*\n",
			want: []int{5432}, wantOK: false,
		},
		{
			name: "netstat LISTEN row with a bad port",
			out: "Active Internet connections (only servers)\n" +
				"tcp 0 0 127.0.0.1:99999 0.0.0.0:* LISTEN\n",
			want: nil, wantOK: false,
		},
		{
			// Empty output is not a table; RemoteListeners decides per
			// command whether empty means "nothing listening" (ss -H).
			name: "empty", out: "",
			want: nil, wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseListeners(tt.out)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ports = %v, want %v", got, tt.want)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
		})
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

	got, ok := parseListeners(out)
	if !ok {
		t.Fatal("parseListeners: output not recognised")
	}
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

func TestSplitHostPort(t *testing.T) {
	tests := []struct {
		in   string
		host string
		port int
		ok   bool
	}{
		{"127.0.0.1:5432", "127.0.0.1", 5432, true},
		{"0.0.0.0:22", "0.0.0.0", 22, true},
		{"*:3330", "*", 3330, true},
		{"[::]:22", "::", 22, true},
		{"[::1]:25", "::1", 25, true},
		{":::8080", "::", 8080, true},
		{"127.0.0.53%lo:53", "127.0.0.53%lo", 53, true},
		// BSD "host.port".
		{"127.0.0.1.3774", "127.0.0.1", 3774, true},
		{"*.3000", "*", 3000, true},
		{"::1.8080", "::1", 8080, true},
		{"fe80::1%lo0.8081", "fe80::1%lo0", 8081, true},
		{"fe80::88b:8cc8:3.53633", "fe80::88b:8cc8:3", 53633, true},
		// Not addresses we can read.
		{"*.*", "", 0, false},
		{"0.0.0.0:*", "", 0, false},
		{"127.0.0.1:0", "", 0, false},
		{"127.0.0.1:65536", "", 0, false},
		{"127.0.0.1:+80", "", 0, false},
		{"somewhere", "", 0, false},
		{".80", "", 0, false},
		{"", "", 0, false},
	}
	for _, tt := range tests {
		host, port, ok := splitHostPort(tt.in)
		if host != tt.host || port != tt.port || ok != tt.ok {
			t.Errorf("splitHostPort(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tt.in, host, port, ok, tt.host, tt.port, tt.ok)
		}
	}
}

// TestLoopbackReachable pins the helper contract: wildcards and loopback
// are reachable through the tunnel, specific addresses are not. Removing
// "*" from the accepted set (the ss dual-stack bug) reddens the first case.
func TestLoopbackReachable(t *testing.T) {
	for host, want := range map[string]bool{
		"*":             true, // ss dual-stack / BSD wildcard
		"0.0.0.0":       true,
		"::":            true,
		"127.0.0.1":     true,
		"127.0.0.53%lo": true,
		"::1":           true,
		"192.168.1.60":  false, // specific LAN IP
		"100.89.53.125": false, // Tailscale
		"fe80::1%lo0":   false, // link-local on lo0: not what 127.0.0.1 reaches
		"":              false,
	} {
		if got := loopbackReachable(host); got != want {
			t.Errorf("loopbackReachable(%q) = %v, want %v", host, got, want)
		}
	}
}
