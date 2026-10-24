// The daemon side of file transfer: sending to the remote's inbox over an SSH
// exec session, and (optionally) receiving into this machine's inbox from a
// listener the remote reaches through a hidden push forward.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/transfer"
	"github.com/Luqueee/mole/internal/tunnel"
)

// A non-interactive SSH command gets a minimal PATH that usually lacks the
// directories mole is installed in (~/.local/bin first of all).
const remoteEnv = `env PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH" `

const (
	remoteRecvCmd     = remoteEnv + "mole recv --stdio"
	remoteSetTokenCmd = remoteEnv + "mole recv --set-token"
)

// streamer runs a command on the remote as a duplex stream; *tunnel.Manager does.
type streamer interface {
	Stream(cmd string) (*tunnel.Stream, error)
}

// hiddenPusher opens mole's own push forwards; *pushForwarder does.
type hiddenPusher interface {
	addHidden(port, remotePort int) error
	removeHidden(port int)
}

type transferHub struct {
	log      *slog.Logger
	remote   string // label of the remote, for logs and offers
	cfgPath  string // config file re-read when it changes ("" = never)
	ssh      streamer
	push     hiddenPusher
	stateDir string
	tlog     *transfer.Log
	gate     *transfer.Gate

	recvCmd, tokenCmd string
	// remotePort is where the receiver shows up on the remote; 0 means the
	// same port as the local listener (tests set it, since both "machines"
	// share one loopback there).
	remotePort int

	ctlMu sync.Mutex // serialises SetReceive, reconnect handling and close
	tokMu sync.Mutex // one token installation at a time

	cfgMu    sync.Mutex
	cfg      config.TransferConfig
	cfgMTime time.Time

	mu        sync.Mutex // guards the fields below; never held across I/O
	wanted    bool       // receiving was asked for (config or live) and should be retried
	receiving bool
	ln        net.Listener
	cancel    context.CancelFunc
	token     string
	port      int
	remoteErr string
}

func newTransferHub(log *slog.Logger, remote, cfgPath string, cfg config.TransferConfig, ssh streamer, push hiddenPusher, stateDir string) *transferHub {
	h := &transferHub{
		log: log, remote: remote, cfgPath: cfgPath, cfg: cfg, ssh: ssh, push: push, stateDir: stateDir,
		tlog: transfer.NewLog(), gate: transfer.NewGate(),
		recvCmd: remoteRecvCmd, tokenCmd: remoteSetTokenCmd,
	}
	if st, err := os.Stat(cfgPath); err == nil {
		h.cfgMTime = st.ModTime()
	}
	return h
}

// settings returns the current transfer settings, re-reading the config file
// if it changed since (so `mole inbox set` takes effect without a restart).
func (h *transferHub) settings() config.TransferConfig {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	if h.cfgPath == "" {
		return h.cfg
	}
	st, err := os.Stat(h.cfgPath)
	if err != nil || st.ModTime().Equal(h.cfgMTime) {
		return h.cfg
	}
	if c, err := config.Load(h.cfgPath); err == nil {
		h.cfg, h.cfgMTime = c.Transfer, st.ModTime()
	}
	return h.cfg
}

// streamConn adapts a remote command to the connection Send expects. Closing
// it ends the command's input and waits briefly for it to finish, so what it
// printed on stderr is complete when an error needs explaining.
type streamConn struct{ *tunnel.Stream }

func (c streamConn) Close() error {
	_ = c.Stream.CloseWrite()
	select {
	case <-c.Stream.Done():
	case <-time.After(2 * time.Second):
	}
	return c.Stream.Close()
}

// Send implements admin.TransferController: it streams body to the remote's
// inbox through `mole recv --stdio` over a fresh SSH session. The session is
// authenticated by SSH itself, so no token is involved.
func (h *transferHub) Send(ctx context.Context, fi transfer.FileInfo, body io.Reader, to string) (transfer.Result, error) {
	st, err := h.ssh.Stream(h.recvCmd)
	if err != nil {
		return transfer.Result{}, fmt.Errorf("open a session on the remote: %w", err)
	}
	f := transfer.File{FileInfo: fi, Open: func() (io.ReadCloser, error) { return io.NopCloser(body), nil }}
	res, err := transfer.Send(ctx, streamConn{st}, []transfer.File{f}, transfer.SendOptions{To: to, Log: h.tlog, Peer: h.remote})
	if err != nil {
		var refused *transfer.RemoteError
		var bad *transfer.BadReplyError
		msg := st.Stderr()
		switch {
		case errors.As(err, &refused) || msg == "":
		case errors.As(err, &bad):
			// Not our protocol at all (a mole too old to know `recv`, or a
			// shell error): what it printed is the useful part.
			err = fmt.Errorf("mole on the remote cannot receive files (it said: %s); update it there with `mole update`", msg)
		default:
			err = fmt.Errorf("%w (the remote said: %s)", err, msg)
		}
		return transfer.Result{}, err
	}
	return res[0], nil
}

// Log implements admin.TransferController.
func (h *transferHub) Log(since uint64) ([]transfer.Record, []transfer.Active, uint64) {
	return h.tlog.Since(since)
}

// Pending implements admin.TransferController.
func (h *transferHub) Pending() []transfer.Pending { return h.gate.List() }

// Decide implements admin.TransferController.
func (h *transferHub) Decide(id uint64, accept bool) error { return h.gate.Decide(id, accept) }

// Info implements admin.TransferController.
func (h *transferHub) Info() map[string]any {
	cfg := h.settings()
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]any{
		"receive":      h.receiving,
		"accept":       cfg.AcceptMode(),
		"inbox":        cfg.InboxDir(),
		"port":         cfg.PortOrDefault(),
		"remote_error": h.remoteErr,
	}
}

func (h *transferHub) setRemoteErr(msg string) {
	h.mu.Lock()
	h.remoteErr = msg
	h.mu.Unlock()
}

// SetReceive implements admin.TransferController: it starts or stops the
// receiver, which listens on loopback and is exposed on the remote by a
// hidden push forward. Turning it on fails atomically: if any step fails,
// nothing is left running.
func (h *transferHub) SetReceive(on bool) error {
	h.ctlMu.Lock()
	defer h.ctlMu.Unlock()
	if !on {
		h.mu.Lock()
		h.wanted = false
		h.mu.Unlock()
		h.stopReceiver()
		return nil
	}
	h.mu.Lock()
	already, tok, port := h.receiving, h.token, h.remotePortLocked()
	h.mu.Unlock()
	if already {
		go h.installToken(tok, port) // refresh the remote's copy
		return nil
	}
	if err := h.startReceiver(); err != nil {
		return err
	}
	h.mu.Lock()
	h.wanted = true
	tok, port = h.token, h.remotePortLocked()
	h.mu.Unlock()
	go h.installToken(tok, port)
	return nil
}

// remotePortLocked is the port the remote's `mole send` must connect to.
func (h *transferHub) remotePortLocked() int {
	if h.remotePort != 0 {
		return h.remotePort
	}
	return h.port
}

func (h *transferHub) startReceiver() error {
	cfg := h.settings()
	tok, err := transfer.LoadOrCreateToken(transfer.TokenPath(h.stateDir))
	if err != nil {
		return fmt.Errorf("transfer token: %w", err)
	}
	port := cfg.PortOrDefault()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listen on 127.0.0.1:%d: %w (change transfer.port)", port, err)
	}
	rcv := &transfer.Receiver{
		Inbox: cfg.InboxDir(), Token: tok, Peer: h.remote, MaxBytes: cfg.MaxBytes(),
		Log: h.tlog, Logger: h.log,
	}
	if cfg.AcceptMode() == "ask" {
		rcv.Approve = func(ctx context.Context, o transfer.Offer) error {
			h.log.Info("incoming transfer waiting for approval", "peer", o.Peer, "files", len(o.Files), "bytes", o.Total, "accept_with", "mole inbox accept")
			return h.gate.Approve(ctx, o)
		}
	}
	remotePort := h.remotePort
	if remotePort == 0 {
		remotePort = port
	}
	if err := h.push.addHidden(port, remotePort); err != nil {
		ln.Close()
		return fmt.Errorf("expose port %d on the remote: %w (is it in use there, or AllowTcpForwarding disabled?)", remotePort, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := rcv.Serve(ctx, ln); err != nil {
			h.log.Warn("transfer receiver stopped", "err", err)
		}
	}()
	h.mu.Lock()
	h.receiving, h.ln, h.cancel, h.token, h.port = true, ln, cancel, tok, port
	h.mu.Unlock()
	h.log.Info("receiving files", "inbox", cfg.InboxDir(), "port", port, "accept", cfg.AcceptMode())
	return nil
}

func (h *transferHub) stopReceiver() {
	h.mu.Lock()
	ln, cancel, port, was := h.ln, h.cancel, h.port, h.receiving
	h.receiving, h.ln, h.cancel = false, nil, nil
	h.mu.Unlock()
	if !was {
		return
	}
	cancel()
	ln.Close()
	h.push.removeHidden(port)
	h.log.Info("stopped receiving files")
}

// installToken writes the shared token (and the receiver's port) on the
// remote, where `mole send` reads them. It needs a mole on the remote that
// knows `recv`; if there is none the reason is kept for the status.
func (h *transferHub) installToken(token string, port int) {
	h.tokMu.Lock()
	defer h.tokMu.Unlock()
	st, err := h.ssh.Stream(h.tokenCmd)
	if err != nil {
		h.setRemoteErr("cannot reach the remote: " + err.Error())
		return
	}
	defer st.Close()
	_, werr := fmt.Fprintf(st, "%s\n%d\n", token, port)
	_ = st.CloseWrite()
	select {
	case <-st.Done():
	case <-time.After(15 * time.Second):
		h.setRemoteErr("timed out installing the transfer token on the remote")
		return
	}
	if err := st.Wait(); err != nil || werr != nil {
		msg := st.Stderr()
		if msg == "" {
			msg = fmt.Sprint(errors.Join(err, werr))
		}
		h.log.Warn("could not install the transfer token on the remote", "err", msg, "hint", "mole on the remote must be installed and up to date (mole update)")
		h.setRemoteErr(msg)
		return
	}
	h.setRemoteErr("")
	h.log.Info("transfer token installed on the remote")
}

// onReconnect runs after the SSH transport was replaced: the hidden push
// forward is reopened by the push forwarder, the remote's token is refreshed,
// and a receiver that failed to start earlier gets another chance.
func (h *transferHub) onReconnect() {
	h.ctlMu.Lock()
	defer h.ctlMu.Unlock()
	h.mu.Lock()
	wanted, receiving, tok, port := h.wanted, h.receiving, h.token, h.remotePortLocked()
	h.mu.Unlock()
	switch {
	case receiving:
		h.installToken(tok, port)
	case wanted:
		if err := h.startReceiver(); err != nil {
			h.log.Warn("could not start receiving files", "err", err)
			return
		}
		h.mu.Lock()
		tok, port = h.token, h.remotePortLocked()
		h.mu.Unlock()
		h.installToken(tok, port)
	}
}

// enableAtStart turns the receiver on when the config asks for it. A failure
// is logged, not fatal; the next reconnect (or `mole inbox receive on`) retries.
func (h *transferHub) enableAtStart() {
	if !h.settings().Receive {
		return
	}
	h.mu.Lock()
	h.wanted = true
	h.mu.Unlock()
	if err := h.SetReceive(true); err != nil {
		h.log.Warn("could not start receiving files", "err", err)
	}
}

func (h *transferHub) close() {
	h.ctlMu.Lock()
	defer h.ctlMu.Unlock()
	h.stopReceiver()
}
