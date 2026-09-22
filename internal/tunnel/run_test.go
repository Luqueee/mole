package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testSSHClient(t *testing.T) sshConn {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		serverSide, err := listener.Accept()
		if err != nil {
			return
		}
		defer serverSide.Close()
		serverConfig := &ssh.ServerConfig{NoClientAuth: true}
		serverConfig.AddHostKey(signer)
		conn, channels, requests, err := ssh.NewServerConn(serverSide, serverConfig)
		if err != nil {
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		var sessions sync.WaitGroup
		for channel := range channels {
			if channel.ChannelType() != "session" {
				_ = channel.Reject(ssh.UnknownChannelType, "session only")
				continue
			}
			sessions.Add(1)
			go func() {
				defer sessions.Done()
				ch, reqs, err := channel.Accept()
				if err != nil {
					return
				}
				defer ch.Close()
				for req := range reqs {
					if req.Type != "exec" {
						_ = req.Reply(false, nil)
						continue
					}
					_ = req.Reply(true, nil)
					_, _ = ch.Write([]byte("remote output\n"))
					status := make([]byte, 4)
					binary.BigEndian.PutUint32(status, 0)
					_, _ = ch.SendRequest("exit-status", false, status)
					return
				}
			}()
		}
		sessions.Wait()
	}()
	clientConfig := &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second}
	client, err := ssh.Dial("tcp", listener.Addr().String(), clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Error("test SSH server did not stop")
		}
	})
	return client
}

func TestManagerRunReconnectsAndReturnsCommandOutput(t *testing.T) {
	client := testSSHClient(t)
	m := &Manager{addr: "test:22", log: discardLogger()}
	m.dialContext = func(context.Context) (sshConn, error) { return client, nil }
	out, err := m.Run("echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "remote output\n" {
		t.Fatalf("Run output = %q", out)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerWatchConnectsAndStops(t *testing.T) {
	client := testSSHClient(t)
	m := &Manager{addr: "test:22", log: discardLogger()}
	connected := make(chan struct{}, 1)
	m.dialContext = func(context.Context) (sshConn, error) { connected <- struct{}{}; return client, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Watch(ctx, time.Millisecond); close(done) }()
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("Watch did not connect")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch did not stop")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}
