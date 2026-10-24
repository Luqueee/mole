package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeHandler struct {
	status    Status
	statusErr error
}

func (f fakeHandler) Hello() Hello {
	return Hello{Agent: ProtocolVersion, Version: "9.9.9", OS: "linux", Arch: "amd64", Hostname: "devlabs", Caps: []string{OpHello, OpStatus}}
}
func (f fakeHandler) Status() (Status, error) { return f.status, f.statusErr }

// duplex returns a client wired to a running Serve.
func duplex(t *testing.T, h Handler) (*Client, <-chan error) {
	t.Helper()
	cr, sw := io.Pipe() // server -> client
	sr, cw := io.Pipe() // client -> server
	served := make(chan error, 1)
	go func() {
		err := Serve(context.Background(), struct {
			io.Reader
			io.Writer
		}{sr, sw}, h)
		sw.Close()
		served <- err
	}()
	c := NewClient(struct {
		io.Reader
		io.Writer
		io.Closer
	}{cr, cw, closerFunc(func() error { cw.Close(); return cr.Close() })})
	t.Cleanup(func() { c.Close() })
	return c, served
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestHelloAndStatus(t *testing.T) {
	c, served := duplex(t, fakeHandler{status: Status{Running: true, Remote: "mac", Nodes: 2, Inbox: "/home/x/mole-inbox", Admin: []byte(`{"stats":{"uptime":"3h"}}`)}})
	ctx := context.Background()
	h, err := c.Hello(ctx)
	if err != nil || h.Agent != ProtocolVersion || h.Version != "9.9.9" || h.Hostname != "devlabs" || len(h.Caps) != 2 {
		t.Fatalf("hello: %+v %v", h, err)
	}
	st, err := c.Status(ctx)
	if err != nil || !st.Running || st.Remote != "mac" || st.Nodes != 2 || !strings.Contains(string(st.Admin), "3h") {
		t.Fatalf("status: %+v %v", st, err)
	}
	// Several calls on one connection keep their order and ids.
	for i := 0; i < 5; i++ {
		if _, err := c.Hello(ctx); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("Serve ended with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return when the client left")
	}
}

func TestStatusError(t *testing.T) {
	c, _ := duplex(t, fakeHandler{statusErr: errors.New("config is broken")})
	_, err := c.Status(context.Background())
	var re *RemoteError
	if !errors.As(err, &re) || re.Msg != "config is broken" {
		t.Fatalf("got %v", err)
	}
	// The connection is still usable after a failed call.
	if _, err := c.Hello(context.Background()); err != nil {
		t.Fatalf("after an error: %v", err)
	}
}

func TestUnsupportedOpAndMalformedLine(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go func() {
		Serve(context.Background(), struct {
			io.Reader
			io.Writer
		}{sr, sw}, fakeHandler{})
		sw.Close()
	}()
	go func() {
		io.WriteString(cw, "\n")         // blank lines are skipped
		io.WriteString(cw, "not json\n") // answered, not fatal
		io.WriteString(cw, `{"id":4,"op":"reboot-the-moon"}`+"\n")
		io.WriteString(cw, `{"id":5,"op":"hello"}`+"\n")
		cw.Close()
	}()
	var out strings.Builder
	io.Copy(&out, cr)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("responses: %q", lines)
	}
	if !strings.Contains(lines[0], "malformed") || !strings.Contains(lines[1], `unsupported op`) || !strings.Contains(lines[1], `"id":4`) || !strings.Contains(lines[2], `"ok":true`) {
		t.Fatalf("responses: %q", lines)
	}
}

func TestClientRecognisesAForeignPeer(t *testing.T) {
	// A mole too old to have an agent prints an error and its usage banner.
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go func() {
		io.Copy(io.Discard, sr)
	}()
	go func() {
		io.WriteString(sw, "\n\n✗ unknown command: agent\n")
		sw.Close()
	}()
	c := NewClient(struct {
		io.Reader
		io.Writer
		io.Closer
	}{cr, cw, closerFunc(func() error { cw.Close(); return cr.Close() })})
	defer c.Close()
	_, err := c.Hello(context.Background())
	var na *ErrNotAgent
	if !errors.As(err, &na) || !strings.Contains(na.Output, "unknown command: agent") {
		t.Fatalf("got %v", err)
	}
	// Silence (the process ended without output) is the same diagnosis.
	cr2, sw2 := io.Pipe()
	sr2, cw2 := io.Pipe()
	go io.Copy(io.Discard, sr2)
	go sw2.Close()
	c2 := NewClient(struct {
		io.Reader
		io.Writer
		io.Closer
	}{cr2, cw2, closerFunc(func() error { cw2.Close(); return cr2.Close() })})
	defer c2.Close()
	if _, err := c2.Hello(context.Background()); !errors.As(err, &na) {
		t.Fatalf("silent peer: %v", err)
	}
}

func TestClientContextCancel(t *testing.T) {
	cr, _ := io.Pipe() // never answers
	_, cw := io.Pipe()
	c := NewClient(struct {
		io.Reader
		io.Writer
		io.Closer
	}{cr, cw, closerFunc(func() error { cw.Close(); return cr.Close() })})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Hello(ctx); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Hello ignored the context")
	}
}
