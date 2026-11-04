package discover

import (
	"errors"
	"testing"
)

// errFakeUnknownCmd simulates the remote not having a requested tool
// (the Runner errors, e.g. "command not found"). RemoteListeners treats
// such an error as "this probe failed, try the next one".
var errFakeUnknownCmd = errors.New("fake: command not found")

// fakeRunner is a Runner whose reply is driven per-command by a table.
// A command absent from responses errors with errFakeUnknownCmd, which
// lets a test model "this tool is missing" simply by omitting it.
type fakeRunner struct {
	responses map[string]fakeResponse
}

type fakeResponse struct {
	out []byte
	err error
}

func (f fakeRunner) Run(cmd string) ([]byte, error) {
	if r, ok := f.responses[cmd]; ok {
		return r.out, r.err
	}
	return nil, errFakeUnknownCmd
}

// Real `ss -tlnH` output (reused style from listeners_test.go): mixed
// IPv4/IPv6 with loopback, wildcard, and non-loopback binds.
const ssSample = `LISTEN 0      4096                       0.0.0.0:9749       0.0.0.0:*
LISTEN 0      100                      127.0.0.1:25         0.0.0.0:*
LISTEN 0      4096                 100.89.53.125:33562      0.0.0.0:*
LISTEN 0      4096                     127.0.0.1:20241      0.0.0.0:*
LISTEN 0      128                        0.0.0.0:22         0.0.0.0:*
LISTEN 0      4096                       0.0.0.0:3301       0.0.0.0:*
LISTEN 0      4096                          [::]:9749          [::]:*
LISTEN 0      100                          [::1]:25            [::]:*
LISTEN 0      128                           [::]:22            [::]:*
LISTEN 0      4096   [fd7a:115c:a1e0::6938:357e]:62391         [::]:*`

// Real `netstat -tln` output with header and trailing LISTEN state.
const netstatSample = `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State
tcp        0      0 0.0.0.0:3301            0.0.0.0:*               LISTEN
tcp        0      0 127.0.0.1:5432          0.0.0.0:*               LISTEN
tcp6       0      0 :::8080                 :::*                    LISTEN`

// Only a Tailscale-IP bind: a successful run that yields zero
// loopback-reachable ports. This is the parse result that must still be
// reported as authoritative.
const ssNonLoopbackOnly = `LISTEN 0      4096                 100.89.53.125:33562      0.0.0.0:*`

func TestRemoteListeners(t *testing.T) {
	tests := []struct {
		name              string
		responses         map[string]fakeResponse
		wantPorts         []int
		wantAuthoritative bool
	}{
		{
			// ss runs and finds loopback ports → those ports, authoritative.
			// netstat is never consulted (early return on len > 0).
			name: "ss succeeds with loopback ports",
			responses: map[string]fakeResponse{
				"ss -tlnH": {out: []byte(ssSample)},
			},
			wantPorts:         []int{22, 25, 3301, 9749, 20241},
			wantAuthoritative: true,
		},
		{
			// ss missing/errors, netstat succeeds → fall back to netstat
			// ports, still authoritative.
			name: "ss errors, netstat succeeds",
			responses: map[string]fakeResponse{
				"ss -tlnH":     {err: errFakeUnknownCmd},
				"netstat -tln": {out: []byte(netstatSample)},
			},
			wantPorts:         []int{3301, 5432, 8080},
			wantAuthoritative: true,
		},
		{
			// Critical "authoritative empty": both tools run cleanly but
			// the remote has nothing loopback-reachable. Must report true
			// so the caller prunes dead forwards rather than re-probing.
			name: "succeeds with zero loopback ports (empty output)",
			responses: map[string]fakeResponse{
				"ss -tlnH":     {out: []byte("")},
				"netstat -tln": {out: []byte("")},
			},
			wantPorts:         nil,
			wantAuthoritative: true,
		},
		{
			// Same authoritative-empty contract via real LISTEN lines that
			// are all bound to a non-loopback address: parsed, found
			// nothing reachable, still authoritative.
			name: "succeeds with only non-loopback binds",
			responses: map[string]fakeResponse{
				"ss -tlnH":     {out: []byte(ssNonLoopbackOnly)},
				"netstat -tln": {out: []byte(ssNonLoopbackOnly)},
			},
			wantPorts:         nil,
			wantAuthoritative: true,
		},
		{
			// Transport down / neither tool present: every command errors.
			// Must report NOT authoritative so the caller keeps existing
			// forwards instead of pruning them away.
			name:              "both ss and netstat error",
			responses:         map[string]fakeResponse{},
			wantPorts:         nil,
			wantAuthoritative: false,
		},
		{
			// ss -H prints nothing on an idle host: the one command whose
			// empty output is a valid "nothing is listening".
			name: "ss empty, netstat missing",
			responses: map[string]fakeResponse{
				"ss -tlnH": {out: []byte("\n")},
			},
			wantPorts:         nil,
			wantAuthoritative: true,
		},
		{
			// netstat always prints a header, so empty output from it
			// proves nothing.
			name: "ss missing, netstat prints nothing",
			responses: map[string]fakeResponse{
				"netstat -an -p tcp": {out: []byte("")},
				"netstat -tln":       {out: []byte("")},
			},
			wantPorts:         nil,
			wantAuthoritative: false,
		},
		{
			// Linux without ss, nothing listening: the header vouches for
			// the empty table.
			name: "netstat header only",
			responses: map[string]fakeResponse{
				"netstat -tln": {out: []byte(fixtureLinuxNetstatNone)},
			},
			wantPorts:         nil,
			wantAuthoritative: true,
		},
		{
			// macOS: no ss; netstat -an -p tcp prints the BSD table.
			name: "macOS netstat -an -p tcp",
			responses: map[string]fakeResponse{
				"netstat -an -p tcp": {out: []byte(fixtureMacNetstatAN)},
				"netstat -tln":       {out: []byte(fixtureMacNetstatTLN)},
			},
			wantPorts:         []int{3000, 3774, 8080, 57621},
			wantAuthoritative: true,
		},
		{
			// The confirmed macOS bug: netstat -tln exits 0 with a table
			// that has no servers in it. It used to be read as an
			// authoritative empty result, pruning every forward; it must
			// fall through to probing instead.
			name: "macOS netstat -tln alone is not authoritative",
			responses: map[string]fakeResponse{
				"netstat -an -p tcp": {err: errFakeUnknownCmd},
				"netstat -tln":       {out: []byte(fixtureMacNetstatTLN)},
			},
			wantPorts:         nil,
			wantAuthoritative: false,
		},
		{
			name: "FreeBSD netstat -an -p tcp",
			responses: map[string]fakeResponse{
				"netstat -an -p tcp": {out: []byte(fixtureFreeBSDNetstatAN)},
			},
			wantPorts:         []int{22, 5432},
			wantAuthoritative: true,
		},
		{
			// Windows OpenSSH: ss is not a command; netstat -tln prints
			// usage; netstat -an -p tcp prints the Windows table.
			name: "windows netstat -an -p tcp",
			responses: map[string]fakeResponse{
				"ss -tlnH":           {out: []byte("'ss' is not recognized as an internal or external command,\r\n"), err: errFakeUnknownCmd},
				"netstat -an -p tcp": {out: []byte(fixtureWindowsNetstat)},
				"netstat -tln":       {out: []byte(fixtureWindowsUsage)},
			},
			wantPorts:         []int{135, 3000, 5040},
			wantAuthoritative: true,
		},
		{
			// Every command "succeeds" but prints something that is not a
			// listener table (a shell that exits 0 on errors, a banner).
			name: "garbage from every command",
			responses: map[string]fakeResponse{
				"ss -tlnH":           {out: []byte("ss : The term 'ss' is not recognized as a cmdlet\n")},
				"netstat -an -p tcp": {out: []byte(fixtureWindowsLocalised)},
				"netstat -tln":       {out: []byte(fixtureWindowsUsage)},
			},
			wantPorts:         nil,
			wantAuthoritative: false,
		},
		{
			// ss output with a LISTEN row we cannot read is not trusted;
			// the next tool's readable table is used instead.
			name: "unreadable ss output falls through to netstat",
			responses: map[string]fakeResponse{
				"ss -tlnH":     {out: []byte("LISTEN 0 4096 somewhere 0.0.0.0:*\n")},
				"netstat -tln": {out: []byte(netstatSample)},
			},
			wantPorts:         []int{3301, 5432, 8080},
			wantAuthoritative: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fakeRunner{responses: tt.responses}
			gotPorts, gotAuthoritative := RemoteListeners(r, quietLogger())

			if !equalInts(gotPorts, tt.wantPorts) {
				t.Errorf("ports = %v, want %v", gotPorts, tt.wantPorts)
			}
			if gotAuthoritative != tt.wantAuthoritative {
				t.Errorf("authoritative = %v, want %v (ports=%v)",
					gotAuthoritative, tt.wantAuthoritative, gotPorts)
			}
		})
	}
}
