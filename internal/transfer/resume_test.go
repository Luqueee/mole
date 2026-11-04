package transfer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// cutFile serves its first limit bytes and then fails, like a link that goes
// down in the middle of the file.
func cutFile(name string, data []byte, limit int, mtime int64) File {
	f := memFile(name, data, 0o644, mtime)
	f.Open = func() (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(bytes.NewReader(data[:limit]), errReader{errors.New("link down")})), nil
	}
	return f
}

func partialsIn(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, n := range listDir(t, dir) {
		if strings.HasSuffix(n, ".part") {
			out = append(out, n)
		}
	}
	return out
}

func inboxFile(t *testing.T, r *Receiver, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Inbox, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInterruptedTransferResumesWhereItStopped(t *testing.T) {
	r := newReceiver(t)
	data := randBytes(t, 3<<20+5)
	const mt = 1_700_000_000
	cut := 1<<20 + 123

	if _, serr, _ := transferOnce(t, r, []File{cutFile("big.bin", data, cut, mt)}, SendOptions{}); serr == nil {
		t.Fatal("the interrupted transfer reported success")
	}
	parts := partialsIn(t, r.Inbox)
	if len(parts) != 1 {
		t.Fatalf("partial files after the interruption: %v, want exactly one", parts)
	}
	if st, err := os.Stat(filepath.Join(r.Inbox, parts[0])); err != nil || st.Size() != int64(cut) {
		t.Fatalf("partial = %v (err %v), want the %d bytes that arrived", st, err, cut)
	}

	res, serr, herr := transferOnce(t, r, []File{memFile("big.bin", data, 0o644, mt)}, SendOptions{})
	if serr != nil || herr != nil {
		t.Fatalf("resumed transfer: send %v, receive %v", serr, herr)
	}
	if len(res) != 1 || res[0].Resumed != int64(cut) {
		t.Fatalf("result = %+v, want resumed from %d", res, cut)
	}
	if !bytes.Equal(inboxFile(t, r, res[0].Saved), data) {
		t.Fatal("the resumed file differs from the original")
	}
	if left := partialsIn(t, r.Inbox); len(left) != 0 {
		t.Fatalf("partial files left after success: %v", left)
	}
}

func TestResumeIgnoresPartialOfAnotherVersionOfTheFile(t *testing.T) {
	r := newReceiver(t)
	data := randBytes(t, 2<<20)
	transferOnce(t, r, []File{cutFile("big.bin", data, 1<<20, 1000)}, SendOptions{})

	// Same name and size, different modification time: not the same content.
	res, serr, herr := transferOnce(t, r, []File{memFile("big.bin", data, 0o644, 2000)}, SendOptions{})
	if serr != nil || herr != nil {
		t.Fatalf("send %v, receive %v", serr, herr)
	}
	if res[0].Resumed != 0 {
		t.Fatalf("resumed %d bytes onto another version of the file", res[0].Resumed)
	}
	if !bytes.Equal(inboxFile(t, r, res[0].Saved), data) {
		t.Fatal("received file differs")
	}
}

func TestCorruptPartialIsDiscardedAndTheNextAttemptStartsOver(t *testing.T) {
	r := newReceiver(t)
	data := randBytes(t, 2<<20)
	const mt = 1000
	if err := os.MkdirAll(r.Inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(r.Inbox, resumeName("big.bin", int64(len(data)), mt))
	if err := os.WriteFile(bad, randBytes(t, 1<<20+10), 0o600); err != nil {
		t.Fatal(err)
	}

	_, serr, _ := transferOnce(t, r, []File{memFile("big.bin", data, 0o644, mt)}, SendOptions{})
	var remote *RemoteError
	if !errors.As(serr, &remote) || !strings.Contains(remote.Msg, "checksum") {
		t.Fatalf("send error = %v, want the receiver's checksum refusal", serr)
	}
	if left := partialsIn(t, r.Inbox); len(left) != 0 {
		t.Fatalf("a corrupt partial survived: %v", left)
	}

	res, serr, herr := transferOnce(t, r, []File{memFile("big.bin", data, 0o644, mt)}, SendOptions{})
	if serr != nil || herr != nil || res[0].Resumed != 0 {
		t.Fatalf("retry: result %+v, send %v, receive %v", res, serr, herr)
	}
	if !bytes.Equal(inboxFile(t, r, res[0].Saved), data) {
		t.Fatal("received file differs")
	}
}

func TestPartialLongerThanTheFileIsNotTrusted(t *testing.T) {
	r := newReceiver(t)
	data := randBytes(t, 2<<20)
	if err := os.MkdirAll(r.Inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(r.Inbox, resumeName("big.bin", int64(len(data)), 1000))
	if err := os.WriteFile(long, randBytes(t, len(data)+10), 0o600); err != nil {
		t.Fatal(err)
	}
	res, serr, herr := transferOnce(t, r, []File{memFile("big.bin", data, 0o644, 1000)}, SendOptions{})
	if serr != nil || herr != nil || res[0].Resumed != 0 {
		t.Fatalf("result %+v, send %v, receive %v", res, serr, herr)
	}
	if !bytes.Equal(inboxFile(t, r, res[0].Saved), data) {
		t.Fatal("received file differs")
	}
}

func TestSmallFilesLeaveNoPartialWhenTheyFail(t *testing.T) {
	r := newReceiver(t)
	data := randBytes(t, 100<<10)
	if _, serr, _ := transferOnce(t, r, []File{cutFile("small.bin", data, 40<<10, 1000)}, SendOptions{}); serr == nil {
		t.Fatal("the interrupted transfer reported success")
	}
	if left := partialsIn(t, r.Inbox); len(left) != 0 {
		t.Fatalf("a small failed transfer left %v", left)
	}
}

// A sender that predates resume does not ask for it: the receiver must not
// offer offsets it cannot honour, nor keep what a failed transfer wrote.
func TestSenderWithoutResumeGetsNoOffsetsAndLeavesNothing(t *testing.T) {
	r := newReceiver(t)
	data := randBytes(t, 2<<20)
	cl, sv := tcpPair(t)
	done := make(chan error, 1)
	go func() {
		err := r.Handle(context.Background(), sv)
		sv.Close()
		done <- err
	}()
	if err := writeJSONLine(cl, hello{V: protoVersion, Files: []FileInfo{{Name: "big.bin", Size: int64(len(data)), Mode: 0o644, MTime: 1000}}}); err != nil {
		t.Fatal(err)
	}
	var rp reply
	if err := readJSONLine(bufio.NewReader(cl), &rp); err != nil || !rp.OK {
		t.Fatalf("reply %+v, err %v", rp, err)
	}
	if rp.Offsets != nil {
		t.Fatalf("offsets %v offered to a sender that did not ask to resume", rp.Offsets)
	}
	if _, err := cl.Write(data[:1<<20]); err != nil {
		t.Fatal(err)
	}
	cl.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("receiver accepted a truncated file")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("receiver did not finish")
	}
	if left := partialsIn(t, r.Inbox); len(left) != 0 {
		t.Fatalf("a non-resuming failed transfer left %v", left)
	}
}

func TestStalePartialsArePrunedButRecentOnesKept(t *testing.T) {
	r := newReceiver(t)
	if err := os.MkdirAll(r.Inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(r.Inbox, resumeName("old.bin", 2<<20, 1))
	recent := filepath.Join(r.Inbox, resumeName("recent.bin", 2<<20, 1))
	for _, p := range []string{old, recent} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-stalePartial - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	if _, serr, herr := transferOnce(t, r, []File{memFile("a.txt", []byte("hi"), 0o644, 1)}, SendOptions{}); serr != nil || herr != nil {
		t.Fatalf("send %v, receive %v", serr, herr)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale partial still there (stat err %v)", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent partial was removed: %v", err)
	}
}

func TestTwoTransfersOfTheSameFileNeverShareAPartial(t *testing.T) {
	dir := t.TempDir()
	first := acquirePartial(dir, "a.bin", 2<<20, 1)
	if !first.resumable {
		t.Fatal("first claim is not resumable")
	}
	second := acquirePartial(dir, "a.bin", 2<<20, 1)
	if second.resumable {
		t.Fatal("second claim shares the first one's partial")
	}
	first.done()
	third := acquirePartial(dir, "a.bin", 2<<20, 1)
	defer third.done()
	if !third.resumable {
		t.Fatal("the partial was not released")
	}
}
