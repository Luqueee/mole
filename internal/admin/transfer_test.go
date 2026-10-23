package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Luqueee/mole/internal/transfer"
)

type fakeTransfer struct {
	sent    transfer.FileInfo
	body    []byte
	to      string
	sendErr error
	decided map[uint64]bool
	receive bool
	recvErr error
}

func (f *fakeTransfer) Send(_ context.Context, fi transfer.FileInfo, body io.Reader, to string) (transfer.Result, error) {
	f.sent, f.to = fi, to
	f.body, _ = io.ReadAll(body)
	if f.sendErr != nil {
		return transfer.Result{}, f.sendErr
	}
	return transfer.Result{Name: fi.Name, Saved: "saved-" + fi.Name, Bytes: int64(len(f.body)), Dir: "/inbox"}, nil
}

func (f *fakeTransfer) Log(since uint64) ([]transfer.Record, []transfer.Active, uint64) {
	return []transfer.Record{{ID: 3, Name: "a", Status: transfer.StatusOK}}, []transfer.Active{{ID: 4, Name: "b", Size: 10, Bytes: 5}}, 4
}

func (f *fakeTransfer) Pending() []transfer.Pending {
	return []transfer.Pending{{ID: 7, Peer: "devlabs", Total: 9}}
}

func (f *fakeTransfer) Decide(id uint64, accept bool) error {
	if id != 7 {
		return transfer.ErrNoPending
	}
	if f.decided == nil {
		f.decided = map[uint64]bool{}
	}
	f.decided[id] = accept
	return nil
}

func (f *fakeTransfer) SetReceive(on bool) error {
	if f.recvErr != nil {
		return f.recvErr
	}
	f.receive = on
	return nil
}

func (f *fakeTransfer) Info() map[string]any {
	return map[string]any{"receive": f.receive, "inbox": "/inbox"}
}

func transferServer(t *testing.T, ft *fakeTransfer) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(NewStats(), map[string]any{"remote": "devlabs"}).WithTransferController(ft).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func put(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTransferSend(t *testing.T) {
	ft := &fakeTransfer{}
	ts := transferServer(t, ft)
	resp := put(t, ts.URL+"/transfer/send?name=a.txt&size=5&mode=420&mtime=1700000000&to=sub", []byte("hello"))
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var res transfer.Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || res.Saved != "saved-a.txt" || res.Bytes != 5 {
		t.Fatalf("result %+v %v", res, err)
	}
	if ft.sent.Name != "a.txt" || ft.sent.Size != 5 || ft.sent.Mode != 420 || ft.sent.MTime != 1700000000 || ft.to != "sub" || string(ft.body) != "hello" {
		t.Fatalf("controller saw %+v to=%q body=%q", ft.sent, ft.to, ft.body)
	}
}

func TestTransferSendRejectsBadRequests(t *testing.T) {
	ts := transferServer(t, &fakeTransfer{})
	for name, url := range map[string]string{
		"no name":       "/transfer/send?size=5",
		"no size":       "/transfer/send?name=a",
		"negative size": "/transfer/send?name=a&size=-1",
		"size mismatch": "/transfer/send?name=a&size=9",
		"non numeric":   "/transfer/send?name=a&size=x",
	} {
		if resp := put(t, ts.URL+url, []byte("hello")); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
}

func TestTransferSendErrorMapping(t *testing.T) {
	ft := &fakeTransfer{sendErr: &transfer.RemoteError{Msg: "no space"}}
	ts := transferServer(t, ft)
	resp := put(t, ts.URL+"/transfer/send?name=a&size=1", []byte("x"))
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(b), "no space") {
		t.Fatalf("remote refusal: %d %s", resp.StatusCode, b)
	}
	ft.sendErr = errors.New("ssh down")
	if resp := put(t, ts.URL+"/transfer/send?name=a&size=1", []byte("x")); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("transport failure: %d", resp.StatusCode)
	}
}

func TestTransferLogPendingDecide(t *testing.T) {
	ft := &fakeTransfer{}
	ts := transferServer(t, ft)

	resp, _ := http.Get(ts.URL + "/transfer/log?since=2")
	var log struct {
		Records []transfer.Record  `json:"records"`
		Active  []transfer.Active  `json:"active"`
		Pending []transfer.Pending `json:"pending"`
		Last    uint64             `json:"last"`
	}
	json.NewDecoder(resp.Body).Decode(&log)
	resp.Body.Close()
	if len(log.Records) != 1 || len(log.Active) != 1 || log.Active[0].Bytes != 5 || len(log.Pending) != 1 || log.Last != 4 {
		t.Fatalf("log: %+v", log)
	}

	resp, _ = http.Get(ts.URL + "/transfer/pending")
	var pend []transfer.Pending
	json.NewDecoder(resp.Body).Decode(&pend)
	resp.Body.Close()
	if len(pend) != 1 || pend[0].ID != 7 {
		t.Fatalf("pending: %+v", pend)
	}

	post := func(path, body string) int {
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("/transfer/pending/7", `{"accept":true}`); c != 200 || !ft.decided[7] {
		t.Fatalf("accept: %d %v", c, ft.decided)
	}
	if c := post("/transfer/pending/7", `{"accept":false}`); c != 200 || ft.decided[7] {
		t.Fatalf("reject: %d %v", c, ft.decided)
	}
	if c := post("/transfer/pending/99", `{"accept":true}`); c != http.StatusNotFound {
		t.Fatalf("unknown id: %d", c)
	}
	if c := post("/transfer/pending/abc", `{"accept":true}`); c != http.StatusBadRequest {
		t.Fatalf("bad id: %d", c)
	}
	if c := post("/transfer/pending/7", `nope`); c != http.StatusBadRequest {
		t.Fatalf("bad body: %d", c)
	}
}

func TestTransferReceiveAndStatus(t *testing.T) {
	ft := &fakeTransfer{}
	ts := transferServer(t, ft)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/transfer/receive", strings.NewReader(`{"on":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 || !ft.receive {
		t.Fatalf("enable: %v %v receive=%v", resp, err, ft.receive)
	}
	resp.Body.Close()

	ft.recvErr = errors.New("port in use")
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/transfer/receive", strings.NewReader(`{"on":true}`))
	resp, _ = http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || !strings.Contains(string(b), "port in use") {
		t.Fatalf("failure: %d %s", resp.StatusCode, b)
	}

	resp, _ = http.Get(ts.URL + "/status")
	var st struct {
		Info struct {
			Remote   string         `json:"remote"`
			Transfer map[string]any `json:"transfer"`
		} `json:"info"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Info.Remote != "devlabs" || st.Info.Transfer["inbox"] != "/inbox" {
		t.Fatalf("status: %+v", st)
	}
}

func TestTransferEndpointsAbsentWithoutController(t *testing.T) {
	ts := httptest.NewServer(New(NewStats(), nil).Handler())
	defer ts.Close()
	resp, _ := http.Get(ts.URL + "/transfer/log")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d", resp.StatusCode)
	}
}
