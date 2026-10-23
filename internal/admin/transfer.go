package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Luqueee/mole/internal/transfer"
)

// TransferController is what the admin API needs from the transfer
// subsystem: sending a file to the remote, the activity log, incoming offers
// waiting for approval, and switching the receiver on and off.
type TransferController interface {
	// Send streams one file to the remote's inbox, optionally into the
	// sub-folder to of it. body yields exactly f.Size bytes.
	Send(ctx context.Context, f transfer.FileInfo, body io.Reader, to string) (transfer.Result, error)
	Log(since uint64) ([]transfer.Record, []transfer.Active, uint64)
	Pending() []transfer.Pending
	Decide(id uint64, accept bool) error
	SetReceive(on bool) error
	// Info is reported under info.transfer in /status.
	Info() map[string]any
}

// WithTransferController enables the /transfer endpoints and reports the
// transfer state in /status.
func (s *Server) WithTransferController(ctl TransferController) *Server {
	s.transferCtl = ctl
	return s
}

func (s *Server) mountTransfer(mux *http.ServeMux) {
	mux.HandleFunc("PUT /transfer/send", s.handleTransferSend)
	mux.HandleFunc("GET /transfer/log", s.handleTransferLog)
	mux.HandleFunc("GET /transfer/pending", s.handleTransferPending)
	mux.HandleFunc("POST /transfer/pending/{id}", s.handleTransferDecide)
	mux.HandleFunc("PUT /transfer/receive", s.handleTransferReceive)
}

// handleTransferSend takes one file as the request body (Content-Length is
// its size) and sends it to the remote's inbox. The file travels through the
// request, never as a path the daemon opens itself, so the API cannot be used
// to make the daemon read files the caller cannot.
//
//	PUT /transfer/send?name=a.txt&size=12&mode=420&mtime=1700000000&to=sub
//
// 200 with the transfer.Result as JSON; 400 bad request; 422 the remote
// refused; 502 any other failure reaching the remote.
func (s *Server) handleTransferSend(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	size, err := strconv.ParseInt(q.Get("size"), 10, 64)
	if name == "" || err != nil || size < 0 {
		http.Error(w, "name and a non-negative size are required", http.StatusBadRequest)
		return
	}
	if r.ContentLength != size {
		http.Error(w, "Content-Length must equal size", http.StatusBadRequest)
		return
	}
	mode, _ := strconv.ParseUint(q.Get("mode"), 10, 32)
	mtime, _ := strconv.ParseInt(q.Get("mtime"), 10, 64)
	fi := transfer.FileInfo{Name: name, Size: size, Mode: uint32(mode), MTime: mtime}
	res, err := s.transferCtl.Send(r.Context(), fi, r.Body, q.Get("to"))
	if err != nil {
		status := http.StatusBadGateway
		var re *transfer.RemoteError
		if errors.As(err, &re) {
			status = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// handleTransferLog serves GET /transfer/log?since=N: the transfers
// finished after N, those in progress, the offers waiting for approval, and
// the newest ID to poll from next time.
func (s *Server) handleTransferLog(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	recs, active, last := s.transferCtl.Log(since)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"records": recs, "active": active, "pending": s.transferCtl.Pending(), "last": last,
	})
}

func (s *Server) handleTransferPending(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.transferCtl.Pending())
}

// handleTransferDecide answers a waiting offer: { "accept": true|false }.
func (s *Server) handleTransferDecide(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Accept bool `json:"accept"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.transferCtl.Decide(id, req.Accept); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, transfer.ErrNoPending) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleTransferReceive switches the receiver on or off: { "on": true|false }.
func (s *Server) handleTransferReceive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.transferCtl.SetReceive(req.On); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
