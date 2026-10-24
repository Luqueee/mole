package transfer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// tcpPair returns the two ends of a real loopback connection. TCP, not
// net.Pipe: the protocol relies on socket buffering (a refusal can be
// written while the sender is still uploading), which a synchronous pipe
// does not provide.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { client.Close(); r.c.Close() })
	return client, r.c
}

func memFile(name string, data []byte, mode uint32, mtime int64) File {
	return File{
		FileInfo: FileInfo{Name: name, Size: int64(len(data)), Mode: mode, MTime: mtime},
		Open:     func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// transferOnce runs one Send against one Handle over a loopback pair.
func transferOnce(t *testing.T, r *Receiver, files []File, opt SendOptions) ([]Result, error, error) {
	t.Helper()
	cl, sv := tcpPair(t)
	herr := make(chan error, 1)
	go func() {
		err := r.Handle(context.Background(), sv)
		sv.Close()
		herr <- err
	}()
	res, serr := Send(context.Background(), cl, files, opt)
	select {
	case e := <-herr:
		return res, serr, e
	case <-time.After(10 * time.Second):
		t.Fatal("receiver did not finish")
		return nil, nil, nil
	}
}

func newReceiver(t *testing.T) *Receiver {
	t.Helper()
	return &Receiver{Inbox: filepath.Join(t.TempDir(), "inbox"), Log: NewLog(), Peer: "devlabs"}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestRoundTrip(t *testing.T) {
	r := newReceiver(t)
	big := randBytes(t, 2<<20+17)
	mt := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC).Unix()
	files := []File{
		memFile("big.bin", big, 0o644, mt),
		memFile("empty.txt", nil, 0o644, mt),
		memFile("run.sh", []byte("#!/bin/sh\n"), 0o755, mt),
	}
	var progress []int64
	res, serr, herr := transferOnce(t, r, files, SendOptions{Peer: "inbox", Log: NewLog(), Progress: func(i int, sent, size int64) {
		if i == 0 {
			progress = append(progress, sent)
		}
	}})
	if serr != nil || herr != nil {
		t.Fatalf("send=%v handle=%v", serr, herr)
	}
	if len(res) != 3 || res[0].Saved != "big.bin" || res[0].Bytes != int64(len(big)) {
		t.Fatalf("results: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(r.Inbox, "big.bin"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("big.bin differs: %v", err)
	}
	if st, _ := os.Stat(filepath.Join(r.Inbox, "empty.txt")); st == nil || st.Size() != 0 {
		t.Fatal("empty file missing or not empty")
	}
	st, err := os.Stat(filepath.Join(r.Inbox, "big.bin"))
	if err != nil || st.ModTime().Unix() != mt {
		t.Fatalf("mtime not preserved: %v %v", st, err)
	}
	if runtime.GOOS != "windows" {
		if m := st.Mode().Perm(); m != 0o600 {
			t.Fatalf("plain file mode %o, want 600 (group/other bits dropped)", m)
		}
		sh, _ := os.Stat(filepath.Join(r.Inbox, "run.sh"))
		if m := sh.Mode().Perm(); m != 0o700 {
			t.Fatalf("script mode %o, want 700", m)
		}
	}
	if len(progress) == 0 || progress[len(progress)-1] != int64(len(big)) {
		t.Fatalf("progress did not reach the file size: %v", progress)
	}
	for _, n := range listDir(t, r.Inbox) {
		if strings.HasSuffix(n, ".part") {
			t.Fatalf("temporary file left behind: %s", n)
		}
	}
	recs, active, _ := r.Log.Since(0)
	if len(recs) != 3 || len(active) != 0 || recs[0].Status != StatusOK || recs[0].Dir != DirRecv || recs[0].Peer != "devlabs" {
		t.Fatalf("receiver log: %+v active=%+v", recs, active)
	}
}

func TestCollisionsGetNewNames(t *testing.T) {
	r := newReceiver(t)
	for i := 0; i < 3; i++ {
		res, serr, herr := transferOnce(t, r, []File{memFile("a.txt", []byte{byte(i)}, 0o644, 0)}, SendOptions{})
		if serr != nil || herr != nil {
			t.Fatal(serr, herr)
		}
		want := []string{"a.txt", "a (1).txt", "a (2).txt"}[i]
		if res[0].Saved != want {
			t.Fatalf("send %d saved as %q, want %q", i, res[0].Saved, want)
		}
	}
	if _, serr, _ := transferOnce(t, r, []File{memFile(".env", []byte("x"), 0o644, 0)}, SendOptions{}); serr != nil {
		t.Fatal(serr)
	}
	res, _, _ := transferOnce(t, r, []File{memFile(".env", []byte("y"), 0o644, 0)}, SendOptions{})
	if res[0].Saved != ".env (1)" {
		t.Fatalf("dotfile collision saved as %q", res[0].Saved)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Inbox, "a.txt")); !bytes.Equal(b, []byte{0}) {
		t.Fatal("an existing file was overwritten")
	}
}

func TestToken(t *testing.T) {
	r := newReceiver(t)
	r.Token = strings.Repeat("ab", 32)
	_, serr, herr := transferOnce(t, r, []File{memFile("x", []byte("x"), 0o644, 0)}, SendOptions{Token: strings.Repeat("cd", 32)})
	var re *RemoteError
	if !errors.As(serr, &re) || re.Msg != "bad token" || herr == nil {
		t.Fatalf("wrong token: send=%v handle=%v", serr, herr)
	}
	if _, err := os.Stat(r.Inbox); err == nil && len(listDir(t, r.Inbox)) != 0 {
		t.Fatal("a file was written for a bad token")
	}
	if _, serr, herr := transferOnce(t, r, []File{memFile("x", []byte("x"), 0o644, 0)}, SendOptions{Token: r.Token}); serr != nil || herr != nil {
		t.Fatalf("right token: %v %v", serr, herr)
	}
}

func TestApproveRefusalAndOffer(t *testing.T) {
	r := newReceiver(t)
	var seen Offer
	r.Approve = func(_ context.Context, o Offer) error {
		seen = o
		return ErrRejected
	}
	slog := NewLog()
	_, serr, _ := transferOnce(t, r, []File{memFile("a", []byte("12345"), 0o644, 0), memFile("b", []byte("123"), 0o644, 0)}, SendOptions{To: "inbox2", Log: slog, Peer: "mac"})
	var re *RemoteError
	if !errors.As(serr, &re) || !strings.Contains(re.Msg, "rejected") {
		t.Fatalf("send error: %v", serr)
	}
	if seen.Total != 8 || len(seen.Files) != 2 || seen.To != "inbox2" || seen.Peer != "devlabs" {
		t.Fatalf("offer: %+v", seen)
	}
	recs, _, _ := r.Log.Since(0)
	if len(recs) != 1 || recs[0].Status != StatusRejected || recs[0].Name != "a (+1 more)" {
		t.Fatalf("receiver log: %+v", recs)
	}
	srecs, _, _ := slog.Since(0)
	if len(srecs) != 1 || srecs[0].Status != StatusFailed || srecs[0].Dir != DirSend {
		t.Fatalf("sender log: %+v", srecs)
	}
	if ents, _ := os.ReadDir(r.Inbox); len(ents) != 0 {
		for _, e := range ents {
			if !e.IsDir() {
				t.Fatalf("file written after refusal: %s", e.Name())
			}
		}
	}
}

func TestOfferIsDroppedWhenTheSenderLeaves(t *testing.T) {
	r := newReceiver(t)
	gate := NewGate()
	r.Approve = gate.Approve
	cl, sv := tcpPair(t)
	herr := make(chan error, 1)
	go func() { herr <- r.Handle(context.Background(), sv); sv.Close() }()
	if err := writeJSONLine(cl, hello{V: 1, Files: []FileInfo{{Name: "gone.txt", Size: 3}}}); err != nil {
		t.Fatal(err)
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 300; i++ {
			if ok() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	eventually("the offer to be listed", func() bool { return len(gate.List()) == 1 })
	cl.Close() // Ctrl+C on the sender
	eventually("the offer to disappear", func() bool { return len(gate.List()) == 0 })
	select {
	case err := <-herr:
		if err == nil || !strings.Contains(err.Error(), "disconnected") {
			t.Fatalf("handle returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Handle kept waiting for an answer nobody wants")
	}
	recs, _, _ := r.Log.Since(0)
	if len(recs) != 1 || recs[0].Status != StatusFailed || !strings.Contains(recs[0].Error, "disconnected") {
		t.Fatalf("log: %+v", recs)
	}
}

func TestAcceptedOfferStillTransfers(t *testing.T) {
	// The watcher that notices a departing sender must not eat the file.
	r := newReceiver(t)
	gate := NewGate()
	r.Approve = gate.Approve
	cl, sv := tcpPair(t)
	go func() { r.Handle(context.Background(), sv); sv.Close() }()
	done := make(chan error, 1)
	var res []Result
	go func() {
		var err error
		res, err = Send(context.Background(), cl, []File{memFile("kept.bin", randBytes(t, 3<<20), 0o644, 0)}, SendOptions{})
		done <- err
	}()
	for i := 0; i < 300 && len(gate.List()) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // the offer sits unanswered for a while
	if err := gate.Decide(gate.List()[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || len(res) != 1 || res[0].Bytes != 3<<20 {
		t.Fatalf("send: %+v %v", res, err)
	}
	if st, err := os.Stat(filepath.Join(r.Inbox, "kept.bin")); err != nil || st.Size() != 3<<20 {
		t.Fatalf("file: %v %v", st, err)
	}
}

func TestMaxBytes(t *testing.T) {
	r := newReceiver(t)
	r.MaxBytes = 10
	_, serr, _ := transferOnce(t, r, []File{memFile("a", make([]byte, 11), 0o644, 0)}, SendOptions{})
	var re *RemoteError
	if !errors.As(serr, &re) || !strings.Contains(re.Msg, "limit") {
		t.Fatalf("got %v", serr)
	}
	if _, serr, _ := transferOnce(t, r, []File{memFile("a", make([]byte, 10), 0o644, 0)}, SendOptions{}); serr != nil {
		t.Fatalf("a transfer at the limit failed: %v", serr)
	}
}

// rawHello sends a hand-built offer and returns the receiver's reply, for
// peers that do not follow Send's good manners.
func rawHello(t *testing.T, r *Receiver, h hello) (reply, error) {
	t.Helper()
	cl, sv := tcpPair(t)
	herr := make(chan error, 1)
	go func() { herr <- r.Handle(context.Background(), sv); sv.Close() }()
	if err := writeJSONLine(cl, h); err != nil {
		t.Fatal(err)
	}
	var rp reply
	if err := readJSONLine(bufio.NewReader(cl), &rp); err != nil {
		t.Fatalf("no reply: %v", err)
	}
	cl.Close()
	return rp, <-herr
}

func TestHostileNamesAndFolders(t *testing.T) {
	r := newReceiver(t)
	outside := filepath.Join(filepath.Dir(r.Inbox), "outside")
	for _, name := range []string{"../escape", "a/b", `a\b`, "", ".", "..", "x\x00y", "tab\tname", strings.Repeat("n", 300), "/etc/passwd"} {
		rp, err := rawHello(t, r, hello{V: 1, Files: []FileInfo{{Name: name, Size: 1}}})
		if rp.OK || err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
	for _, to := range []string{"..", "../up", "a/../../b", `a\b`, strings.Repeat("a/", 20), "x/\x00"} {
		rp, err := rawHello(t, r, hello{V: 1, To: to, Files: []FileInfo{{Name: "f", Size: 1}}})
		if rp.OK || err == nil {
			t.Errorf("folder %q was accepted", to)
		}
	}
	for _, f := range []FileInfo{{Name: "f", Size: -1}, {Name: "f", Size: maxFileSize + 1}} {
		if rp, _ := rawHello(t, r, hello{V: 1, Files: []FileInfo{f}}); rp.OK {
			t.Errorf("size %d accepted", f.Size)
		}
	}
	if rp, _ := rawHello(t, r, hello{V: 1}); rp.OK {
		t.Error("an offer without files was accepted")
	}
	if rp, _ := rawHello(t, r, hello{V: 2, Files: []FileInfo{{Name: "f", Size: 1}}}); rp.OK {
		t.Error("an unknown protocol version was accepted")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("something was created outside the inbox")
	}
}

func TestSubdir(t *testing.T) {
	r := newReceiver(t)
	res, serr, herr := transferOnce(t, r, []File{memFile("r.pdf", []byte("pdf"), 0o644, 0)}, SendOptions{To: "reports/2026"})
	if serr != nil || herr != nil {
		t.Fatal(serr, herr)
	}
	if _, err := os.Stat(filepath.Join(r.Inbox, "reports", "2026", "r.pdf")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(res[0].Dir), "reports/2026") {
		t.Fatalf("dir reported as %q", res[0].Dir)
	}
}

func TestSymlinkCannotEscapeInbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	r := newReceiver(t)
	if err := os.MkdirAll(r.Inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(r.Inbox, "link")); err != nil {
		t.Fatal(err)
	}
	_, serr, _ := transferOnce(t, r, []File{memFile("pwn", []byte("x"), 0o644, 0)}, SendOptions{To: "link"})
	if serr == nil {
		t.Fatal("a symlinked folder let the file out of the inbox")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("file written outside the inbox: %v", ents)
	}
}

// manualSender speaks the protocol by hand so a test can break it.
func manualSender(t *testing.T, r *Receiver, h hello, body func(c net.Conn)) (reply, error) {
	t.Helper()
	cl, sv := tcpPair(t)
	herr := make(chan error, 1)
	go func() { herr <- r.Handle(context.Background(), sv); sv.Close() }()
	br := bufio.NewReader(cl)
	if err := writeJSONLine(cl, h); err != nil {
		t.Fatal(err)
	}
	var rp reply
	if err := readJSONLine(br, &rp); err != nil || !rp.OK {
		t.Fatalf("offer refused: %v %+v", err, rp)
	}
	body(cl)
	var last reply
	_ = readJSONLine(br, &last)
	cl.Close()
	return last, <-herr
}

func TestCorruptionIsRejectedAndCleaned(t *testing.T) {
	r := newReceiver(t)
	data := []byte("hello world")
	last, herr := manualSender(t, r, hello{V: 1, Files: []FileInfo{{Name: "f.txt", Size: int64(len(data))}}}, func(c net.Conn) {
		c.Write(data)
		bad := sha256.Sum256([]byte("something else"))
		c.Write(bad[:])
	})
	if last.OK || herr == nil || !strings.Contains(last.Error, "checksum") {
		t.Fatalf("reply=%+v err=%v", last, herr)
	}
	if names := listDir(t, r.Inbox); len(names) != 0 {
		t.Fatalf("leftovers after corruption: %v", names)
	}
	recs, _, _ := r.Log.Since(0)
	if len(recs) != 1 || recs[0].Status != StatusFailed {
		t.Fatalf("log: %+v", recs)
	}
}

func TestTruncatedStreamLeavesNothing(t *testing.T) {
	r := newReceiver(t)
	_, herr := manualSender(t, r, hello{V: 1, Files: []FileInfo{{Name: "f.bin", Size: 1000}}}, func(c net.Conn) {
		c.Write(make([]byte, 400))
		c.Close() // the connection drops mid-file
	})
	if herr == nil {
		t.Fatal("receiver accepted a truncated file")
	}
	if names := listDir(t, r.Inbox); len(names) != 0 {
		t.Fatalf("leftovers after a cut transfer: %v", names)
	}
}

func TestSenderFileShrinksWhileSending(t *testing.T) {
	r := newReceiver(t)
	f := memFile("f", []byte("short"), 0o644, 0)
	f.Size = 100 // promised 100 bytes, the file only has 5
	_, serr, _ := transferOnce(t, r, []File{f}, SendOptions{})
	if serr == nil || !strings.Contains(serr.Error(), "changed") {
		t.Fatalf("got %v", serr)
	}
	if names := listDir(t, r.Inbox); len(names) != 0 {
		t.Fatalf("leftovers: %v", names)
	}
}

func TestSecondFileFailureKeepsFirst(t *testing.T) {
	r := newReceiver(t)
	bad := File{FileInfo: FileInfo{Name: "bad", Size: 3}, Open: func() (io.ReadCloser, error) { return nil, errors.New("permission denied") }}
	res, serr, _ := transferOnce(t, r, []File{memFile("good", []byte("ok"), 0o644, 0), bad}, SendOptions{})
	if serr == nil || len(res) != 1 || res[0].Saved != "good" {
		t.Fatalf("res=%+v err=%v", res, serr)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Inbox, "good")); string(b) != "ok" {
		t.Fatal("first file lost")
	}
}

func serveOneReply(t *testing.T, reply string, closeNow bool) net.Conn {
	t.Helper()
	cl, sv := tcpPair(t)
	go func() {
		bufio.NewReader(sv).ReadString('\n')
		io.WriteString(sv, reply)
		if closeNow {
			sv.Close()
		}
	}()
	return cl
}

func TestSendDetectsPeerThatIsNotMole(t *testing.T) {
	_, err := Send(context.Background(), serveOneReply(t, "bash: mole: command not found\n", true), []File{memFile("a", []byte("a"), 0o644, 0)}, SendOptions{})
	var bad *BadReplyError
	if !errors.As(err, &bad) || !strings.Contains(bad.Line, "command not found") {
		t.Fatalf("got %v", err)
	}
	_, err = Send(context.Background(), serveOneReply(t, "", true), []File{memFile("a", []byte("a"), 0o644, 0)}, SendOptions{})
	if err == nil || !strings.Contains(err.Error(), "without replying") {
		t.Fatalf("got %v", err)
	}
}

func TestSendSkipsBlankLinesBeforeTheReply(t *testing.T) {
	// A peer that is not mole may print blank lines first; a real receiver
	// answering after them still works.
	cl, sv := tcpPair(t)
	go func() {
		br := bufio.NewReader(sv)
		br.ReadString('\n')
		io.WriteString(sv, "\n\n")
		writeJSONLine(sv, reply{OK: true, Dir: "/x"})
		io.CopyN(io.Discard, br, 1+sha256.Size)
		writeJSONLine(sv, reply{OK: true, Saved: "a", Bytes: 1})
	}()
	res, err := Send(context.Background(), cl, []File{memFile("a", []byte("a"), 0o644, 0)}, SendOptions{})
	if err != nil || len(res) != 1 || res[0].Saved != "a" {
		t.Fatalf("%+v %v", res, err)
	}
	// Only blank lines: give up with a clear error rather than waiting forever.
	cl2 := serveOneReply(t, strings.Repeat("\n", 20), false)
	if _, err := Send(context.Background(), cl2, []File{memFile("a", []byte("a"), 0o644, 0)}, SendOptions{}); err == nil {
		t.Fatal("blank lines were taken for a reply")
	}
}

func TestSendHearsRefusalWhileUploading(t *testing.T) {
	cl, sv := tcpPair(t)
	go func() {
		br := bufio.NewReader(sv)
		br.ReadString('\n')
		writeJSONLine(sv, reply{OK: true, Dir: "/x"})
		io.CopyN(io.Discard, br, 1024)
		writeJSONLine(sv, reply{Error: "no space left on device"})
		sv.(*net.TCPConn).CloseWrite()
		sv.SetReadDeadline(time.Now().Add(5 * time.Second))
		io.Copy(io.Discard, sv)
	}()
	data := make([]byte, 128<<20) // far beyond the socket buffers
	done := make(chan error, 1)
	go func() {
		_, err := Send(context.Background(), cl, []File{memFile("big", data, 0o644, 0)}, SendOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		var re *RemoteError
		if !errors.As(err, &re) || !strings.Contains(re.Msg, "no space") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send kept uploading after the receiver refused")
	}
}

func TestSendContextCancel(t *testing.T) {
	r := newReceiver(t)
	started := make(chan struct{})
	r.Approve = func(ctx context.Context, _ Offer) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	cl, sv := tcpPair(t)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go func() { r.Handle(rctx, sv); sv.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Send(ctx, cl, []File{memFile("a", []byte("a"), 0o644, 0)}, SendOptions{})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send ignored cancellation")
	}
}

func TestServeConcurrentSameName(t *testing.T) {
	r := newReceiver(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	served := make(chan error, 1)
	go func() { served <- r.Serve(context.Background(), ln) }()

	const n = 6
	var wg sync.WaitGroup
	saved := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			res, err := Send(context.Background(), c, []File{memFile("same.txt", []byte(fmt.Sprint("content-", i)), 0o644, 0)}, SendOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			saved <- res[0].Saved
		}(i)
	}
	wg.Wait()
	close(saved)
	seen := map[string]bool{}
	for s := range saved {
		if seen[s] {
			t.Fatalf("two transfers saved as %q", s)
		}
		seen[s] = true
	}
	if len(seen) != n {
		t.Fatalf("saved %d of %d", len(seen), n)
	}
	contents := map[string]bool{}
	for s := range seen {
		b, err := os.ReadFile(filepath.Join(r.Inbox, s))
		if err != nil {
			t.Fatal(err)
		}
		contents[string(b)] = true
	}
	if len(contents) != n {
		t.Fatal("a file was overwritten by a concurrent transfer")
	}
	ln.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after the listener closed")
	}
}

func TestServeRefusalReachesSenderEvenWhenUploading(t *testing.T) {
	r := newReceiver(t)
	r.Token = strings.Repeat("ab", 32)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go r.Serve(context.Background(), ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Send(context.Background(), c, []File{memFile("big", make([]byte, 64<<20), 0o644, 0)}, SendOptions{Token: "wrong"})
		done <- err
	}()
	select {
	case err := <-done:
		var re *RemoteError
		if !errors.As(err, &re) || re.Msg != "bad token" {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hung")
	}
}

func TestGarbageOffer(t *testing.T) {
	r := newReceiver(t)
	cl, sv := tcpPair(t)
	herr := make(chan error, 1)
	go func() { herr <- r.Handle(context.Background(), sv); sv.Close() }()
	io.WriteString(cl, "GET / HTTP/1.1\r\n")
	var rp reply
	if err := readJSONLine(bufio.NewReader(cl), &rp); err != nil || rp.OK {
		t.Fatalf("reply %+v err %v", rp, err)
	}
	if <-herr == nil {
		t.Fatal("garbage was accepted")
	}
	// An endless line must not be buffered without bound.
	cl2, sv2 := tcpPair(t)
	go func() { herr <- r.Handle(context.Background(), sv2); sv2.Close() }()
	go func() {
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 8; i++ {
			if _, err := cl2.Write(chunk); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-herr:
		if err == nil {
			t.Fatal("an endless line was accepted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("receiver buffered an endless line")
	}
}

func TestCleanName(t *testing.T) {
	good := []string{"a.txt", "my file (1).pdf", "ñandú.txt", ".bashrc", "a..b"}
	for _, n := range good {
		got, err := CleanName(n)
		if err != nil {
			t.Errorf("%q: %v", n, err)
		}
		if runtime.GOOS != "windows" && got != n {
			t.Errorf("%q became %q", n, got)
		}
	}
	for _, n := range []string{"", ".", "..", "a/b", `a\b`, "a\x00", "a\nb", "\xff\xfe", strings.Repeat("x", 256)} {
		if _, err := CleanName(n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestWindowsSafe(t *testing.T) {
	cases := map[string]string{"a:b?.txt": "a_b_.txt", "CON": "_CON", "nul.txt": "_nul.txt", "dots...": "dots", "   ": "_", "ok.txt": "ok.txt"}
	for in, want := range cases {
		if got := windowsSafe(in); got != want {
			t.Errorf("windowsSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanSubdir(t *testing.T) {
	if got, err := CleanSubdir(""); err != nil || got != nil {
		t.Fatal(got, err)
	}
	got, err := CleanSubdir("/a/b/")
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"..", "a/..", "a//b", "a/./b", strings.Repeat("a/", 9) + "a"} {
		if _, err := CleanSubdir(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGate(t *testing.T) {
	g := NewGate()
	ctx := context.Background()
	res := make(chan error, 1)
	go func() {
		res <- g.Approve(ctx, Offer{Peer: "devlabs", Total: 5, Files: []FileInfo{{Name: "a", Size: 5}}})
	}()
	var p []Pending
	for i := 0; i < 100 && len(p) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
		p = g.List()
	}
	if len(p) != 1 || p[0].Peer != "devlabs" || p[0].Total != 5 {
		t.Fatalf("pending: %+v", p)
	}
	if err := g.Decide(p[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := <-res; err != nil {
		t.Fatalf("accepted offer failed: %v", err)
	}
	if len(g.List()) != 0 {
		t.Fatal("decided offer still listed")
	}
	if err := g.Decide(p[0].ID, true); !errors.Is(err, ErrNoPending) {
		t.Fatalf("got %v", err)
	}

	go func() { res <- g.Approve(ctx, Offer{}) }()
	for i := 0; i < 100 && len(g.List()) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	g.Decide(g.List()[0].ID, false)
	if err := <-res; !errors.Is(err, ErrRejected) {
		t.Fatalf("got %v", err)
	}

	g2 := &Gate{Timeout: 50 * time.Millisecond, items: map[uint64]*gateItem{}}
	if err := g2.Approve(ctx, Offer{}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("got %v", err)
	}

	g3 := &Gate{Max: 1, Timeout: time.Second, items: map[uint64]*gateItem{}}
	cctx, cancel := context.WithCancel(ctx)
	go g3.Approve(cctx, Offer{})
	for i := 0; i < 100 && len(g3.List()) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if err := g3.Approve(ctx, Offer{}); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("got %v", err)
	}
	cancel()
}

func TestToken_RoundTripAndPerms(t *testing.T) {
	p := TokenPath(filepath.Join(t.TempDir(), "state"))
	a, err := LoadOrCreateToken(p)
	if err != nil || !ValidToken(a) {
		t.Fatal(a, err)
	}
	b, err := LoadOrCreateToken(p)
	if err != nil || a != b {
		t.Fatalf("token changed between loads: %q vs %q (%v)", a, b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode %o", st.Mode().Perm())
		}
	}
	if err := WriteToken(p, "short"); err == nil {
		t.Fatal("invalid token written")
	}
	os.WriteFile(p, []byte("garbage\n"), 0o600)
	c, err := LoadOrCreateToken(p)
	if err != nil || !ValidToken(c) || c == a {
		t.Fatalf("a corrupt token file was not replaced: %q %v", c, err)
	}
}

func TestFileFromPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.txt")
	os.WriteFile(p, []byte("hey"), 0o640)
	f, err := FileFromPath(p)
	if err != nil || f.Name != "x.txt" || f.Size != 3 {
		t.Fatal(f, err)
	}
	rc, _ := f.Open()
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hey" {
		t.Fatal(string(b))
	}
	if _, err := FileFromPath(dir); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory: %v", err)
	}
	if _, err := FileFromPath(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestLogRing(t *testing.T) {
	l := NewLog()
	for i := 0; i < maxRecords+10; i++ {
		l.Add(Record{Name: fmt.Sprint(i)})
	}
	recs, _, last := l.Since(0)
	if len(recs) != maxRecords || last != uint64(maxRecords+10) || recs[0].Name != "10" {
		t.Fatalf("ring: len=%d last=%d first=%s", len(recs), last, recs[0].Name)
	}
	tr := l.Begin(DirSend, "p", "f", 100)
	tr.Write(make([]byte, 40))
	_, act, _ := l.Since(0)
	if len(act) != 1 || act[0].Bytes != 40 || act[0].Size != 100 {
		t.Fatalf("active: %+v", act)
	}
	tr.Done(StatusOK, nil, "f")
	recs, act, _ = l.Since(last)
	if len(recs) != 1 || recs[0].Bytes != 40 || len(act) != 0 {
		t.Fatalf("after done: %+v %+v", recs, act)
	}
	var nl *Log
	nl.Add(Record{})
	nt := nl.Begin("", "", "", 0)
	nt.Write([]byte("x"))
	nt.Done(StatusOK, nil, "")
	_ = json.Marshal
}
