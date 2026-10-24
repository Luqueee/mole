package transfer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Receiver writes the files a peer sends into Inbox.
type Receiver struct {
	// Inbox is the absolute folder files land in; it is created on demand.
	Inbox string
	// Token, when set, must be presented by the sender. Leave it empty
	// when the connection is already authenticated (an SSH session).
	Token string
	// Peer labels the other side in logs and offers.
	Peer string
	// MaxBytes caps the size of one transfer (0 = no cap).
	MaxBytes int64
	// Approve, when set, is asked before anything is written; returning an
	// error refuses the transfer.
	Approve func(ctx context.Context, o Offer) error
	Log     *Log
	Logger  *slog.Logger
}

const (
	idleTimeout    = 2 * time.Minute
	maxConcurrent  = 8
	drainTimeout   = 3 * time.Second
	drainByteLimit = 1 << 30
)

func (r *Receiver) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Receiver) peer() string {
	if r.Peer != "" {
		return r.Peer
	}
	return "peer"
}

// Handle serves one connection: it reads the sender's offer and files and
// answers as it goes. It does not close conn.
func (r *Receiver) Handle(ctx context.Context, conn io.ReadWriter) error {
	br := bufio.NewReaderSize(conn, 64<<10)
	var h hello
	if err := readJSONLine(br, &h); err != nil {
		var bad *BadReplyError
		if errors.As(err, &bad) {
			_ = writeJSONLine(conn, reply{Error: "not a mole transfer"})
		}
		return fmt.Errorf("read offer: %w", err)
	}
	refuse := func(msg string) error {
		_ = writeJSONLine(conn, reply{Error: msg})
		return errors.New(msg)
	}
	if h.V != protoVersion {
		return refuse(fmt.Sprintf("unsupported protocol version %d", h.V))
	}
	if r.Token != "" && subtle.ConstantTimeCompare([]byte(h.Token), []byte(r.Token)) != 1 {
		return refuse("bad token")
	}
	if len(h.Files) == 0 || len(h.Files) > maxFiles {
		return refuse("invalid number of files")
	}
	var total int64
	names := make([]string, len(h.Files))
	for i, f := range h.Files {
		name, err := CleanName(f.Name)
		if err != nil {
			return refuse(err.Error())
		}
		if f.Size < 0 || f.Size > maxFileSize {
			return refuse(fmt.Sprintf("invalid size for %q", f.Name))
		}
		names[i] = name
		total += f.Size
	}
	if r.MaxBytes > 0 && total > r.MaxBytes {
		return refuse(fmt.Sprintf("transfer of %d bytes exceeds the limit of %d bytes", total, r.MaxBytes))
	}
	subs, err := CleanSubdir(h.To)
	if err != nil {
		return refuse(err.Error())
	}
	dir, err := r.prepareDir(subs)
	if err != nil {
		r.logger().Warn("transfer: cannot prepare inbox", "err", err)
		return refuse("the receiver cannot write to its inbox")
	}
	summary := names[0]
	if len(names) > 1 {
		summary = fmt.Sprintf("%s (+%d more)", summary, len(names)-1)
	}
	if r.Approve != nil {
		if err := r.Approve(ctx, Offer{Peer: r.peer(), To: h.To, Files: h.Files, Total: total}); err != nil {
			r.Log.Add(Record{Dir: DirRecv, Peer: r.peer(), Name: summary, Size: total, Status: StatusRejected, Error: err.Error(), StartMs: time.Now().UnixMilli()})
			return refuse(err.Error())
		}
	}
	if err := writeJSONLine(conn, reply{OK: true, Dir: dir}); err != nil {
		return err
	}
	for i, f := range h.Files {
		if err := r.receiveOne(br, conn, dir, names[i], f); err != nil {
			return err
		}
	}
	return nil
}

// prepareDir creates the inbox (and sub-folders) and checks that, symlinks
// resolved, the destination is still inside it.
func (r *Receiver) prepareDir(subs []string) (string, error) {
	if !filepath.IsAbs(r.Inbox) {
		return "", fmt.Errorf("inbox %q is not an absolute path", r.Inbox)
	}
	if err := os.MkdirAll(r.Inbox, 0o755); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(r.Inbox)
	if err != nil {
		return "", err
	}
	dir := root
	for _, s := range subs {
		dir = filepath.Join(dir, s)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", fmt.Errorf("destination %q escapes the inbox", dir)
	}
	return real, nil
}

var errChecksum = errors.New("checksum mismatch: the file was corrupted in transit")

func (r *Receiver) receiveOne(br *bufio.Reader, conn io.Writer, dir, name string, fi FileInfo) (err error) {
	tr := r.Log.Begin(DirRecv, r.peer(), name, fi.Size)
	saved := ""
	defer func() {
		if err != nil {
			tr.Done(StatusFailed, err, "")
			_ = writeJSONLine(conn, reply{Error: err.Error()})
			return
		}
		tr.Done(StatusOK, nil, saved)
	}()

	tmp, err := os.CreateTemp(dir, ".mole-*.part")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h, tr), io.LimitReader(br, fi.Size))
	if err == nil && n != fi.Size {
		err = io.ErrUnexpectedEOF
	}
	var sum [sha256.Size]byte
	if err == nil {
		_, err = io.ReadFull(br, sum[:])
	}
	if err == nil && !bytes.Equal(sum[:], h.Sum(nil)) {
		err = errChecksum
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}

	perm := os.FileMode(0o600)
	if fi.Mode&0o100 != 0 {
		perm |= 0o100
	}
	_ = os.Chmod(tmpName, perm)
	if fi.MTime > 0 {
		mt := time.Unix(fi.MTime, 0)
		_ = os.Chtimes(tmpName, mt, mt)
	}
	final, saved2, err := claimName(dir, name)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(final)
		return err
	}
	published = true
	saved = saved2
	return writeJSONLine(conn, reply{OK: true, Saved: saved, Bytes: n})
}

// claimName reserves a free name in dir (adding " (1)", " (2)" before the
// extension when taken) by creating it exclusively, so two simultaneous
// transfers of the same name cannot overwrite each other. The caller
// renames the finished file over the placeholder.
func claimName(dir, name string) (path, saved string, err error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" { // dotfile such as ".bashrc"
		stem, ext = name, ""
	}
	for i := 0; i < 10000; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		p := filepath.Join(dir, cand)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return p, cand, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("no free name for %q", name)
}

type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}

func (c *idleConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Serve accepts connections on ln until it is closed, handling up to
// maxConcurrent at once. It returns nil once ln is closed.
func (r *Receiver) Serve(ctx context.Context, ln net.Listener) error {
	sem := make(chan struct{}, maxConcurrent)
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
				return nil
			}
			r.logger().Warn("transfer: accept failed", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			c.Close() // too many at once
			continue
		}
		go func() {
			defer func() { <-sem }()
			r.serveConn(ctx, c)
		}()
	}
}

func (r *Receiver) serveConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	ic := &idleConn{Conn: c, idle: idleTimeout}
	if err := r.Handle(ctx, ic); err != nil {
		r.logger().Warn("transfer: receive failed", "peer", r.peer(), "err", err)
		// Closing with unread data would reset the connection and lose the
		// refusal on its way to the sender: end our side, then soak up what
		// the sender is still pushing.
		_ = ic.CloseWrite()
		_ = c.SetReadDeadline(time.Now().Add(drainTimeout))
		_, _ = io.Copy(io.Discard, io.LimitReader(c, drainByteLimit))
	}
}
