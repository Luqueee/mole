package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Luqueee/mole/internal/meshdns"
	"github.com/Luqueee/mole/internal/proxy"
	"github.com/Luqueee/mole/internal/tunnel"
	"golang.org/x/crypto/ssh"
)

func dnsQuery(t *testing.T, addr, name string, qtype uint16) (rcode int, answers []net.IP) {
	t.Helper()
	q := []byte{0x12, 0x34, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(name, ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = binary.BigEndian.AppendUint16(append(q, 0), qtype)
	q = binary.BigEndian.AppendUint16(q, 1)
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	r := buf[:n]
	if r[0] != 0x12 || r[1] != 0x34 || r[2]&0x80 == 0 {
		t.Fatalf("bad header % x", r[:12])
	}
	for i := 0; i < int(binary.BigEndian.Uint16(r[6:8])); i++ {
		answers = append(answers, net.IP(r[n-4*(i+1):n-4*i]))
	}
	return int(r[3] & 0x0f), answers
}

// Names under the domain get a virtual IP, and connecting to it is forwarded
// through real SSH *by name* (the server sees the hostname, not an IP).
func TestMeshResolvesOnRemote(t *testing.T) {
	requireLoopbackRange(t)
	t.Setenv("SSH_AUTH_SOCK", "")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	keyFile := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newSSHTestServer(t, signer.PublicKey())
	backend := localEcho(t)
	srv.backend = net.JoinHostPort("127.0.0.1", strconv.Itoa(backend))

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := tunnel.New(tunnel.Remote{User: "u", Addr: srv.ln.Addr().String(), IdentityFiles: []string{keyFile}}, true, log)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	bindPort := freeLoopbackPort(t)
	port := bindPort - meshdns.PortShift // domain port; mole listens on port+PortShift
	mesh := meshdns.New([]string{"kena.lan"}, []int{port}, mgr, proxy.Hooks{}, log)
	defer mesh.Close()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go mesh.ServeUDP(pc)
	addr := pc.LocalAddr().String()

	rc, ips := dnsQuery(t, addr, "Admin.Kena.Lan", 1)
	if rc != 0 || len(ips) != 1 || !strings.HasPrefix(ips[0].String(), "127.77.") {
		t.Fatalf("A: rc=%d ips=%v", rc, ips)
	}
	if _, again := dnsQuery(t, addr, "admin.kena.lan", 1); again[0].String() != ips[0].String() {
		t.Fatalf("not stable: %v vs %v", again, ips)
	}
	if rc, ips := dnsQuery(t, addr, "admin.kena.lan", 28); rc != 0 || len(ips) != 0 {
		t.Fatalf("AAAA: rc=%d ips=%v", rc, ips)
	}
	if rc, _ := dnsQuery(t, addr, "example.com", 1); rc != 5 {
		t.Fatalf("out of domain rcode = %d, want REFUSED", rc)
	}

	if got := dialPing(ips[0].String(), bindPort); got != "pong" {
		t.Fatalf("through mesh: %q", got)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.hosts) != 1 || srv.hosts[0] != "admin.kena.lan:"+strconv.Itoa(port) {
		t.Fatalf("remote saw %v, want hostname forwarded", srv.hosts)
	}
}

func dialPing(ip string, port int) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil {
		return ""
	}
	return string(b)
}

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
