package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// fakeSource is a remote machine holding one file.
type fakeSource struct {
	data     []byte
	mtime    int64
	badSum   bool  // answer a checksum that does not match
	noSum    bool  // the machine has no way to compute one
	sumErr   error // fail the checksum request
	cutAfter int   // fail an Open stream after this many bytes; <0 = never
	opens    []int64
	sums     int
}

func newFakeSource(data []byte) *fakeSource {
	return &fakeSource{data: data, mtime: 1_700_000_000, cutAfter: -1}
}

func (s *fakeSource) Stat(context.Context, string) (FileInfo, error) {
	return FileInfo{Name: "big.bin", Size: int64(len(s.data)), Mode: 0o644, MTime: s.mtime}, nil
}

func (s *fakeSource) Open(_ context.Context, _ string, off int64) (io.ReadCloser, error) {
	s.opens = append(s.opens, off)
	rest := s.data[off:]
	if s.cutAfter >= 0 && s.cutAfter < len(rest) {
		return io.NopCloser(io.MultiReader(bytes.NewReader(rest[:s.cutAfter]), errReader{errors.New("link down")})), nil
	}
	return io.NopCloser(bytes.NewReader(rest)), nil
}

func (s *fakeSource) Sum(context.Context, string) (string, bool, error) {
	s.sums++
	switch {
	case s.sumErr != nil:
		return "", false, s.sumErr
	case s.noSum:
		return "", false, nil
	case s.badSum:
		return hex.EncodeToString(make([]byte, sha256.Size)), true, nil
	}
	sum := sha256.Sum256(s.data)
	return hex.EncodeToString(sum[:]), true, nil
}

func pullDir(t *testing.T) string { return filepath.Join(t.TempDir(), "downloads") }

func readSaved(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPullDownloadsAndVerifiesTheWholeFile(t *testing.T) {
	data := randBytes(t, 2<<20+99)
	dir := pullDir(t)
	var last int64
	res, err := Pull(context.Background(), newFakeSource(data), "/srv/big.bin", dir, PullOptions{Progress: func(got, size int64) {
		if size != int64(len(data)) || got < last {
			t.Errorf("progress went from %d to %d of %d", last, got, size)
		}
		last = got
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified || res.Resumed != 0 || res.Saved != "big.bin" || res.Size != int64(len(data)) {
		t.Fatalf("result = %+v", res)
	}
	if last != int64(len(data)) {
		t.Fatalf("progress ended at %d, want %d", last, len(data))
	}
	if !bytes.Equal(readSaved(t, dir, res.Saved), data) {
		t.Fatal("downloaded file differs")
	}
	if left := partialsIn(t, dir); len(left) != 0 {
		t.Fatalf("partial files left: %v", left)
	}
	st, _ := os.Stat(filepath.Join(dir, res.Saved))
	if st.ModTime().Unix() != 1_700_000_000 {
		t.Fatalf("modification time = %v, want the source's", st.ModTime())
	}
}

func TestPullResumesAnInterruptedDownload(t *testing.T) {
	data := randBytes(t, 3<<20+5)
	dir := pullDir(t)
	src := newFakeSource(data)
	cut := 1<<20 + 7

	src.cutAfter = cut
	if _, err := Pull(context.Background(), src, "/srv/big.bin", dir, PullOptions{}); err == nil {
		t.Fatal("the interrupted download reported success")
	}
	if parts := partialsIn(t, dir); len(parts) != 1 {
		t.Fatalf("partial files after the interruption: %v", parts)
	}

	src.cutAfter = -1
	res, err := Pull(context.Background(), src, "/srv/big.bin", dir, PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resumed != int64(cut) || !res.Verified {
		t.Fatalf("result = %+v, want resumed from %d and verified", res, cut)
	}
	if len(src.opens) != 2 || src.opens[0] != 0 || src.opens[1] != int64(cut) {
		t.Fatalf("the source was read from offsets %v, want [0 %d]", src.opens, cut)
	}
	if !bytes.Equal(readSaved(t, dir, res.Saved), data) {
		t.Fatal("resumed file differs")
	}
}

func TestPullDiscardsTheDownloadWhenTheChecksumDiffers(t *testing.T) {
	data := randBytes(t, 2<<20)
	dir := pullDir(t)
	src := newFakeSource(data)
	src.badSum = true
	if _, err := Pull(context.Background(), src, "/srv/big.bin", dir, PullOptions{}); !errors.Is(err, errPullMismatch) {
		t.Fatalf("error = %v, want the checksum mismatch", err)
	}
	if left := listDir(t, dir); len(left) != 0 {
		t.Fatalf("a rejected download left %v", left)
	}

	src.badSum = false
	res, err := Pull(context.Background(), src, "/srv/big.bin", dir, PullOptions{})
	if err != nil || res.Resumed != 0 {
		t.Fatalf("retry: %+v, %v", res, err)
	}
}

func TestPullKeepsTheDataWhenOnlyTheVerificationFailed(t *testing.T) {
	data := randBytes(t, 2<<20)
	dir := pullDir(t)
	src := newFakeSource(data)
	src.sumErr = errors.New("sha256sum: Input/output error")
	if _, err := Pull(context.Background(), src, "/srv/big.bin", dir, PullOptions{}); err == nil {
		t.Fatal("a failed verification reported success")
	}

	src.sumErr = nil
	res, err := Pull(context.Background(), src, "/srv/big.bin", dir, PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resumed != int64(len(data)) || !res.Verified {
		t.Fatalf("result = %+v, want the whole file kept and verified", res)
	}
	if len(src.opens) != 1 {
		t.Fatalf("the source was streamed %d times, want only the first", len(src.opens))
	}
}

func TestPullWithoutChecksumSupportOrWhenSkipped(t *testing.T) {
	data := randBytes(t, 2<<20)
	src := newFakeSource(data)
	src.noSum = true
	res, err := Pull(context.Background(), src, "/srv/big.bin", pullDir(t), PullOptions{})
	if err != nil || res.Verified {
		t.Fatalf("no checksum available: %+v, %v", res, err)
	}

	src = newFakeSource(data)
	res, err = Pull(context.Background(), src, "/srv/big.bin", pullDir(t), PullOptions{NoVerify: true})
	if err != nil || res.Verified || src.sums != 0 {
		t.Fatalf("NoVerify: %+v, %v, %d checksum requests", res, err, src.sums)
	}
}

func TestPullNeverReplacesAnExistingFile(t *testing.T) {
	dir := pullDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := randBytes(t, 1<<20)
	res, err := Pull(context.Background(), newFakeSource(data), "/srv/big.bin", dir, PullOptions{})
	if err != nil || res.Saved != "big (1).bin" {
		t.Fatalf("result %+v, err %v, want big (1).bin", res, err)
	}
	if got := readSaved(t, dir, "big.bin"); string(got) != "mine" {
		t.Fatalf("the existing file was overwritten: %q", got)
	}
}
