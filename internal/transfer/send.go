package transfer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Result is the outcome of one sent file.
type Result struct {
	Name  string `json:"name"`
	Saved string `json:"saved"` // final name at the destination
	Bytes int64  `json:"bytes"`
	Dir   string `json:"dir"` // folder the receiver wrote into
}

// SendOptions tunes Send. All fields are optional.
type SendOptions struct {
	Token string // shared secret, for receivers that require one
	To    string // sub-folder of the receiver's inbox
	// Progress is called for every chunk written: file index, bytes of
	// that file sent so far, its size. It runs on the sending goroutine.
	Progress func(file int, sent, size int64)
	Log      *Log   // records each file
	Peer     string // label for Log
}

type progressWriter struct {
	fn         func(int, int64, int64)
	idx        int
	size, sent int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.sent += int64(len(b))
	if p.fn != nil {
		p.fn(p.idx, p.sent, p.size)
	}
	return len(b), nil
}

// Send offers files to the receiver at the other end of conn and streams
// them one after another, stopping at the first failure. Send always closes
// conn, whether it succeeds or not. Cancelling ctx aborts the transfer.
func Send(ctx context.Context, conn io.ReadWriteCloser, files []File, opt SendOptions) ([]Result, error) {
	if len(files) == 0 {
		conn.Close()
		return nil, errors.New("no files to send")
	}
	if len(files) > maxFiles {
		conn.Close()
		return nil, fmt.Errorf("too many files (%d, the limit is %d)", len(files), maxFiles)
	}
	infos := make([]FileInfo, len(files))
	for i, f := range files {
		infos[i] = f.FileInfo
	}

	var once sync.Once
	closeConn := func() { once.Do(func() { _ = conn.Close() }) }
	defer closeConn()
	stop := context.AfterFunc(ctx, closeConn)
	defer stop()

	// Replies are read concurrently with the upload: a receiver that
	// refuses mid-file (disk full, bad checksum) must be heard before the
	// whole file has been pushed into a connection it no longer reads.
	replies := make(chan reply, len(files)+1)
	readErr := make(chan error, 1)
	br := bufio.NewReader(conn)
	go func() {
		defer close(replies)
		for i := 0; i < len(files)+1; i++ {
			var rp reply
			if err := readJSONLine(br, &rp); err != nil {
				readErr <- err
				return
			}
			replies <- rp
			if !rp.OK {
				closeConn() // unblocks an upload stuck on a connection nobody reads
				return
			}
		}
	}()
	next := func() (reply, error) {
		select {
		case rp, ok := <-replies:
			if ok {
				return rp, nil
			}
			select {
			case err := <-readErr:
				return reply{}, err
			default:
				return reply{}, io.ErrUnexpectedEOF
			}
		case <-ctx.Done():
			return reply{}, ctx.Err()
		}
	}
	// explain turns a write failure into the receiver's own refusal when it
	// managed to send one before dropping the connection.
	explain := func(werr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case rp, ok := <-replies:
			if ok && !rp.OK {
				return &RemoteError{Msg: rp.Error}
			}
		case <-time.After(500 * time.Millisecond):
		}
		return werr
	}
	summary := files[0].Name
	if len(files) > 1 {
		summary = fmt.Sprintf("%s (+%d more)", summary, len(files)-1)
	}
	fail := func(err error, size int64) error {
		opt.Log.Add(Record{Dir: DirSend, Peer: opt.Peer, Name: summary, Size: size, Status: StatusFailed, Error: err.Error(), StartMs: time.Now().UnixMilli()})
		return err
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}

	if err := writeJSONLine(conn, hello{V: protoVersion, Token: opt.Token, To: opt.To, Files: infos}); err != nil {
		return nil, fail(explain(err), total)
	}
	rp, err := next()
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = errors.New("the other side closed the connection without replying (is mole up to date there?)")
		}
		return nil, fail(err, total)
	}
	if !rp.OK {
		return nil, fail(&RemoteError{Msg: rp.Error}, total)
	}
	dir := rp.Dir

	results := make([]Result, 0, len(files))
	buf := make([]byte, 256<<10)
	for i, f := range files {
		tr := opt.Log.Begin(DirSend, opt.Peer, f.Name, f.Size)
		res, err := func() (Result, error) {
			rc, err := f.Open()
			if err != nil {
				return Result{}, err
			}
			defer rc.Close()
			h := sha256.New()
			w := io.MultiWriter(conn, h, tr, &progressWriter{fn: opt.Progress, idx: i, size: f.Size})
			n, err := io.CopyBuffer(w, io.LimitReader(rc, f.Size), buf)
			if err != nil {
				return Result{}, explain(err)
			}
			if n != f.Size {
				return Result{}, fmt.Errorf("%s changed while it was being sent", f.Name)
			}
			if _, err := conn.Write(h.Sum(nil)); err != nil {
				return Result{}, explain(err)
			}
			rp, err := next()
			if err != nil {
				return Result{}, err
			}
			if !rp.OK {
				return Result{}, &RemoteError{Msg: rp.Error}
			}
			return Result{Name: f.Name, Saved: rp.Saved, Bytes: rp.Bytes, Dir: dir}, nil
		}()
		if err != nil {
			tr.Done(StatusFailed, err, "")
			return results, err
		}
		tr.Done(StatusOK, nil, res.Saved)
		results = append(results, res)
	}
	return results, nil
}
