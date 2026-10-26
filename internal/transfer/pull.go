package transfer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Source is a file on another machine that this one downloads.
type Source interface {
	// Stat describes the regular file at path. Its Name is the base name.
	Stat(ctx context.Context, path string) (FileInfo, error)
	// Open streams the file's bytes from offset to its end.
	Open(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
	// Sum returns the hex SHA-256 of the whole file. ok is false when the
	// machine cannot compute one, which is not an error.
	Sum(ctx context.Context, path string) (sum string, ok bool, err error)
}

// PullOptions tunes Pull. All fields are optional.
type PullOptions struct {
	// NoVerify skips the checksum comparison with the source.
	NoVerify bool
	// Progress is called as bytes arrive: bytes of the file held so far
	// (including the part kept from an earlier attempt) and its size.
	Progress func(got, size int64)
	Log      *Log
	Peer     string
}

// PullResult is the outcome of one downloaded file.
type PullResult struct {
	Name     string // name the file has at the source
	Saved    string // final name in the destination folder
	Size     int64
	Resumed  int64 // leading bytes kept from an earlier attempt
	Verified bool  // the source confirmed the SHA-256 of the whole file
}

var errPullMismatch = errors.New("the file differs from the source's checksum: it changed while downloading or arrived corrupted; the partial was discarded")

type progressFunc func(int64)

func (f progressFunc) Write(p []byte) (int, error) {
	f(int64(len(p)))
	return len(p), nil
}

// Pull downloads the file at path from src into dir. An interrupted
// download keeps what arrived and the next call for the same file continues
// from there; the final checksum covers the whole file either way.
func Pull(ctx context.Context, src Source, path, dir string, opt PullOptions) (res PullResult, err error) {
	fi, err := src.Stat(ctx, path)
	if err != nil {
		return res, err
	}
	name, err := CleanName(fi.Name)
	if err != nil {
		return res, err
	}
	if fi.Size < 0 || fi.Size > maxFileSize {
		return res, fmt.Errorf("invalid size for %q", fi.Name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	pruneStale(dir)

	p := acquirePartial(dir, name, fi.Size, fi.MTime)
	defer p.done()

	tr := opt.Log.Begin(DirRecv, opt.Peer, name, fi.Size)
	defer func() {
		if err != nil {
			tr.Done(StatusFailed, err, "")
		}
	}()

	f, h, err := p.open(dir)
	if err != nil {
		return res, err
	}
	closed := false
	published := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if !published {
			p.abandon(err)
		}
	}()

	res = PullResult{Name: fi.Name, Size: fi.Size, Resumed: p.off}
	tr.Skip(p.off)
	got := p.off
	report := func() {
		if opt.Progress != nil {
			opt.Progress(got, fi.Size)
		}
	}
	report()

	if want := fi.Size - p.off; want > 0 {
		rc, err := src.Open(ctx, path, p.off)
		if err != nil {
			return res, err
		}
		defer rc.Close()
		w := io.MultiWriter(f, h, tr, progressFunc(func(n int64) { got += n; report() }))
		n, err := io.Copy(w, io.LimitReader(rc, want))
		if err == nil && n != want {
			err = fmt.Errorf("the source ended after %d of %d bytes: %w", p.off+n, fi.Size, io.ErrUnexpectedEOF)
		}
		if err != nil {
			return res, err
		}
	}
	closed = true
	if err := f.Close(); err != nil {
		return res, err
	}

	if !opt.NoVerify {
		sum, ok, err := src.Sum(ctx, path)
		if err != nil {
			return res, fmt.Errorf("verify checksum: %w", err)
		}
		if ok {
			if !strings.EqualFold(strings.TrimSpace(sum), hex.EncodeToString(h.Sum(nil))) {
				p.discard()
				return res, errPullMismatch
			}
			res.Verified = true
		}
	}

	saved, err := p.publish(dir, name, fi)
	if err != nil {
		return res, err
	}
	published = true
	res.Saved = saved
	tr.Done(StatusOK, nil, saved)
	return res, nil
}
