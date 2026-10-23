package tunnel

import (
	"context"
	"testing"
	"time"
)

func TestOnReconnectFiresAfterRedial(t *testing.T) {
	m := &Manager{addr: "test:22", log: discardLogger(), client: newDeadConn(0)}
	m.dialContext = func(context.Context) (sshConn, error) { return &trackedLiveConn{}, nil }
	fired := make(chan struct{}, 2)
	m.OnReconnect(func() { fired <- struct{}{} })

	if _, err := m.Dial("tcp", "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("OnReconnect did not fire")
	}
}

func TestListenNotConnected(t *testing.T) {
	m := &Manager{addr: "test:22", log: discardLogger()}
	if _, err := m.Listen("tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("expected error without a client")
	}
	// Fakes without Listen support must not panic either.
	m.client = newDeadConn(0)
	if _, err := m.Listen("tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("expected error for client without Listen")
	}
}
