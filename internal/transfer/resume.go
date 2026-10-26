package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// minResumeSize is the smallest file worth resuming: below it a failed
	// transfer just starts over, and no hidden partial is left behind.
	minResumeSize = 1 << 20

	// stalePartial is how long an abandoned partial is kept before the next
	// transfer into the same folder removes it.
	stalePartial = 7 * 24 * time.Hour

	resumePrefix = ".mole-resume-"
	resumeSuffix = ".part"
)

var errPartialChanged = errors.New("the partial file changed while the transfer was starting: run it again")

// inUse holds the stable partial paths some transfer in this process is
// writing, so two transfers of the same file never share one.
var inUse = struct {
	mu sync.Mutex
	m  map[string]struct{}
}{m: map[string]struct{}{}}

func claimPath(path string) bool {
	inUse.mu.Lock()
	defer inUse.mu.Unlock()
	if _, busy := inUse.m[path]; busy {
		return false
	}
	inUse.m[path] = struct{}{}
	return true
}

func unclaimPath(path string) {
	inUse.mu.Lock()
	delete(inUse.m, path)
	inUse.mu.Unlock()
}

// resumeName is the partial's file name. It depends on everything that
// identifies the content, so a file that changed at the source never resumes
// onto the bytes of its previous version.
func resumeName(name string, size, mtime int64) string {
	sum := sha256.Sum256([]byte(name + "\x00" + strconv.FormatInt(size, 10) + "\x00" + strconv.FormatInt(mtime, 10)))
	return resumePrefix + hex.EncodeToString(sum[:8]) + resumeSuffix
}

// partial is the file a transfer writes before publishing it under its real
// name. A resumable one has a stable name and survives a failed transfer; any
// other one is a throwaway temporary file.
type partial struct {
	path      string
	off       int64 // bytes already on disk and trusted as the start of the file
	resumable bool
}

// acquirePartial prepares the partial for a file of size bytes and reports
// how much of it is already present. The caller must call done.
func acquirePartial(dir, name string, size, mtime int64) *partial {
	if size < minResumeSize {
		return &partial{}
	}
	path := filepath.Join(dir, resumeName(name, size, mtime))
	if !claimPath(path) {
		return &partial{} // another transfer of this very file is running
	}
	p := &partial{path: path, resumable: true}
	switch st, err := os.Lstat(path); {
	case err != nil:
	case st.Mode().IsRegular() && st.Size() <= size:
		p.off = st.Size()
	default:
		_ = os.Remove(path) // not ours, or longer than the file: unusable
	}
	return p
}

// done releases the claim on a resumable partial. Safe on a throwaway one.
func (p *partial) done() {
	if p != nil && p.resumable {
		unclaimPath(p.path)
	}
}

// open returns the partial's file positioned where writing continues,
// together with a hash that already covers the bytes kept from before.
func (p *partial) open(dir string) (*os.File, hash.Hash, error) {
	h := sha256.New()
	if !p.resumable {
		f, err := os.CreateTemp(dir, ".mole-*.part")
		if err != nil {
			return nil, nil, fmt.Errorf("create temporary file: %w", err)
		}
		p.path = f.Name()
		return f, h, nil
	}
	f, err := os.OpenFile(p.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("create partial file: %w", err)
	}
	if p.off > 0 {
		st, err := f.Stat()
		if err == nil && st.Size() < p.off {
			err = errPartialChanged
		}
		if err == nil {
			err = f.Truncate(p.off)
		}
		if err == nil {
			var n int64
			n, err = io.Copy(h, io.LimitReader(f, p.off))
			if err == nil && n != p.off {
				err = errPartialChanged
			}
		}
		if err != nil {
			f.Close()
			p.discard()
			return nil, nil, err
		}
	}
	if _, err := f.Seek(p.off, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, h, nil
}

// discard deletes the partial file.
func (p *partial) discard() {
	if p.path != "" {
		_ = os.Remove(p.path)
	}
}

// abandon is called when the transfer failed with err. A resumable partial
// stays for the next attempt unless the data itself is bad.
func (p *partial) abandon(err error) {
	if !p.resumable || errors.Is(err, errChecksum) || errors.Is(err, errPartialChanged) {
		p.discard()
	}
}

// publish gives the finished partial its real name in dir, never replacing a
// file that is already there.
func (p *partial) publish(dir, name string, fi FileInfo) (saved string, err error) {
	perm := os.FileMode(0o600)
	if fi.Mode&0o100 != 0 {
		perm |= 0o100
	}
	_ = os.Chmod(p.path, perm)
	if fi.MTime > 0 {
		mt := time.Unix(fi.MTime, 0)
		_ = os.Chtimes(p.path, mt, mt)
	}
	final, saved, err := claimName(dir, name)
	if err != nil {
		return "", err
	}
	if err := os.Rename(p.path, final); err != nil {
		_ = os.Remove(final)
		return "", err
	}
	return saved, nil
}

// pruneStale removes resumable partials in dir that nobody has touched for
// stalePartial and that no transfer of this process is using.
func pruneStale(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-stalePartial)
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, resumePrefix) || !strings.HasSuffix(n, resumeSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, n)
		if claimPath(path) {
			_ = os.Remove(path)
			unclaimPath(path)
		}
	}
}
