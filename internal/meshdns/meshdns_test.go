package meshdns

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"testing"

	"github.com/Luqueee/mole/internal/proxy"
)

// requireLoopbackRange skips the test where only 127.0.0.1 is loopback (macOS
// and the BSDs). Split DNS hands out addresses from 127.77.0.0/16, which Linux
// and Windows treat as loopback in full; there the feature cannot work, and a
// failed bind says nothing about the code under test.
func requireLoopbackRange(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.77.0.2:0")
	if err != nil {
		t.Skipf("127.77.0.0/16 is not usable as loopback on this system: %v", err)
	}
	ln.Close()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func query(m *Mesh, name string) (rcode byte, answers uint16) {
	q := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range []string{name[:len(name)-len(".kena.lan")], "kena", "lan"} {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = binary.BigEndian.AppendUint16(append(q, 0), 1)
	q = binary.BigEndian.AppendUint16(q, 1)
	r := m.Answer(q)
	return r[3] & 0x0f, binary.BigEndian.Uint16(r[6:8])
}

// A virtual IP must never be published when one of its ports is owned by
// another local process (a wildcard listener also covers 127.77.x.y):
// clients would silently reach that process instead of the remote.
func TestResolveIsAllOrNothingOnPortConflict(t *testing.T) {
	requireLoopbackRange(t)
	if runtime.GOOS == "windows" {
		// There a listener on 127.77.x.y coexists with a wildcard one and wins
		// for its own address, so the conflict this guards against cannot happen.
		t.Skip("a wildcard listener does not conflict with a specific one on Windows")
	}
	busy, err := net.Listen("tcp", "0.0.0.0:0") // wildcard, like docker-proxy
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port - PortShift // domain port whose shifted bind port is busy
	okPort := freePort(t) - PortShift

	m := New([]string{"kena.lan"}, []int{okPort, busyPort}, &net.Dialer{}, proxy.Hooks{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()

	if rc, an := query(m, "admin.kena.lan"); rc != rcodeFail || an != 0 {
		t.Fatalf("rcode=%d answers=%d, want SERVFAIL and no answer", rc, an)
	}
	if len(m.List()) != 0 {
		t.Fatalf("name cached despite failure: %v", m.List())
	}
	// The port bound before the failure must have been released.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.77.0.2", strconv.Itoa(BindPort(okPort))))
	if err != nil {
		t.Fatalf("listener leaked after failed resolve: %v", err)
	}
	ln.Close()

	// Once the conflict is gone the same name resolves normally.
	busy.Close()
	if rc, an := query(m, "admin.kena.lan"); rc != rcodeOK || an != 1 {
		t.Fatalf("after conflict cleared: rcode=%d answers=%d", rc, an)
	}
}

type fakeRoutes map[string]bool

func (f fakeRoutes) Lookup(name string) (string, bool) { return "", f[name] }

// A route name is served by the Host proxy behind RouteIP: it must not allocate
// a virtual IP or bind anything, so it works where 127.77.0.0/16 does not.
func TestRouteNamesAnswerRouteIP(t *testing.T) {
	m := New([]string{"mole.lan"}, []int{freePort(t)}, nil, proxy.Hooks{}, slog.New(slog.DiscardHandler))
	m.SetRoutes(fakeRoutes{"a.mole.lan": true})

	q := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range []string{"a", "mole", "lan"} {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = binary.BigEndian.AppendUint16(append(q, 0), 1)
	q = binary.BigEndian.AppendUint16(q, 1)

	r := m.Answer(q)
	if r[3]&0x0f != 0 || binary.BigEndian.Uint16(r[6:8]) != 1 {
		t.Fatalf("rcode=%d answers=%d, want an answer", r[3]&0x0f, binary.BigEndian.Uint16(r[6:8]))
	}
	if ip := net.IP(r[len(r)-4:]); !ip.Equal(net.ParseIP(RouteIP)) {
		t.Errorf("answered %v, want %s", ip, RouteIP)
	}
	if got := m.List(); len(got) != 0 {
		t.Errorf("route name allocated a virtual IP: %+v", got)
	}
}
