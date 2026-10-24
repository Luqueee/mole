package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/Luqueee/mole/internal/transfer"
)

// TransferView is what the Transfers tab renders. Supported is false when
// the daemon predates file transfer.
type TransferView struct {
	Supported bool               `json:"supported"`
	Last      uint64             `json:"last"`
	Records   []transfer.Record  `json:"records"`
	Active    []transfer.Active  `json:"active"`
	Pending   []transfer.Pending `json:"pending"`
}

// SendResult is the outcome of sending one file.
type SendResult struct {
	Name  string `json:"name"`
	Saved string `json:"saved"`
	Dir   string `json:"dir"`
	Error string `json:"error,omitempty"`
}

// sendFolder is the sub-folder of the remote's inbox that dropped files go
// to; the frontend keeps it in sync with its input.
type sendFolder struct {
	mu sync.Mutex
	to string
}

var folder sendFolder

// SetSendFolder sets where dropped files are put inside the remote's inbox.
func (m *MoleService) SetSendFolder(to string) {
	folder.mu.Lock()
	folder.to = strings.TrimSpace(to)
	folder.mu.Unlock()
}

func (m *MoleService) sendFolder() string {
	folder.mu.Lock()
	defer folder.mu.Unlock()
	return folder.to
}

// GetTransfers returns the transfers finished after since, those in
// progress and the offers waiting for an answer.
func (m *MoleService) GetTransfers(since uint64) TransferView {
	none := TransferView{Records: []transfer.Record{}, Active: []transfer.Active{}, Pending: []transfer.Pending{}}
	cfg, _, err := loadConfig()
	if err != nil || cfg.AdminAddr == "" {
		return none
	}
	resp, err := m.client.Get("http://" + cfg.AdminAddr + "/transfer/log?since=" + strconv.FormatUint(since, 10))
	if err != nil {
		return none
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return none
	}
	var body struct {
		Records []transfer.Record  `json:"records"`
		Active  []transfer.Active  `json:"active"`
		Pending []transfer.Pending `json:"pending"`
		Last    uint64             `json:"last"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return none
	}
	v := TransferView{Supported: true, Last: body.Last, Records: body.Records, Active: body.Active, Pending: body.Pending}
	if v.Records == nil {
		v.Records = []transfer.Record{}
	}
	if v.Active == nil {
		v.Active = []transfer.Active{}
	}
	if v.Pending == nil {
		v.Pending = []transfer.Pending{}
	}
	return v
}

// PickFiles opens the system file dialog and returns the chosen paths
// (none if the user cancels).
func (m *MoleService) PickFiles() ([]string, error) {
	return application.Get().Dialog.OpenFile().
		SetTitle("Send files to the remote").
		CanChooseFiles(true).
		CanChooseDirectories(false).
		PromptForMultipleSelection()
}

// SendFiles sends each file to the remote's inbox through the daemon, one
// after another. A file that fails does not stop the others.
func (m *MoleService) SendFiles(paths []string, to string) []SendResult {
	out := make([]SendResult, 0, len(paths))
	cfg, _, err := loadConfig()
	for _, p := range paths {
		name := filepath.Base(p)
		switch {
		case err != nil:
			out = append(out, SendResult{Name: name, Error: err.Error()})
		case cfg.AdminAddr == "":
			out = append(out, SendResult{Name: name, Error: "admin_addr is not set in the mole config"})
		default:
			res, serr := uploadFile(context.Background(), cfg.AdminAddr, p, strings.TrimSpace(to))
			if serr != nil {
				out = append(out, SendResult{Name: name, Error: serr.Error()})
				continue
			}
			out = append(out, SendResult{Name: name, Saved: res.Saved, Dir: res.Dir})
		}
	}
	return out
}

// sendDropped sends files dropped on the window and reports through the log
// (the Transfers tab shows the progress and the outcome).
func (m *MoleService) sendDropped(paths []string) {
	for _, r := range m.SendFiles(paths, m.sendFolder()) {
		if r.Error != "" {
			application.Get().Logger.Warn("dropped file not sent", "name", r.Name, "err", r.Error)
		}
	}
}

// uploadFile streams one file to the daemon, which relays it to the remote.
// The file's bytes travel in the request, never its path.
func uploadFile(ctx context.Context, addr, path, to string) (transfer.Result, error) {
	f, err := transfer.FileFromPath(path)
	if err != nil {
		return transfer.Result{}, err
	}
	rc, err := f.Open()
	if err != nil {
		return transfer.Result{}, err
	}
	defer rc.Close()
	q := url.Values{}
	q.Set("name", f.Name)
	q.Set("size", strconv.FormatInt(f.Size, 10))
	q.Set("mode", strconv.FormatUint(uint64(f.Mode), 10))
	q.Set("mtime", strconv.FormatInt(f.MTime, 10))
	if to != "" {
		q.Set("to", to)
	}
	var body io.Reader = io.LimitReader(rc, f.Size)
	if f.Size == 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+addr+"/transfer/send?"+q.Encode(), body)
	if err != nil {
		return transfer.Result{}, err
	}
	req.ContentLength = f.Size
	resp, err := (&http.Client{}).Do(req) // no timeout: a transfer takes as long as it takes
	if err != nil {
		return transfer.Result{}, fmt.Errorf("mole is not running: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return transfer.Result{}, errors.New(strings.TrimSpace(string(msg)))
	}
	var res transfer.Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return transfer.Result{}, fmt.Errorf("bad reply from the daemon: %w", err)
	}
	return res, nil
}

// ResolveTransfer accepts or rejects an incoming offer.
func (m *MoleService) ResolveTransfer(id uint64, accept bool) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	return m.adminJSON(cfg.AdminAddr, http.MethodPost, "/transfer/pending/"+strconv.FormatUint(id, 10), map[string]bool{"accept": accept})
}

// SetReceive turns on or off the acceptance of files sent from the remote,
// and remembers the choice in the config.
func (m *MoleService) SetReceive(on bool) error {
	if err := m.mole("inbox", "receive", map[bool]string{true: "on", false: "off"}[on]); err != nil {
		return err
	}
	return nil
}

// OpenInbox shows the inbox folder in the system file manager.
func (m *MoleService) OpenInbox() error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	dir := cfg.Transfer.InboxDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", dir)
	case "windows":
		cmd = exec.Command("explorer", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	return cmd.Start()
}

// pendingCount is how many incoming offers wait for an answer (the tray
// shows it). Zero when the daemon is down or predates file transfer.
func (m *MoleService) pendingCount() int {
	cfg, _, err := loadConfig()
	if err != nil || cfg.AdminAddr == "" {
		return 0
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + cfg.AdminAddr + "/transfer/pending")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var p []transfer.Pending
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&p) != nil {
		return 0
	}
	return len(p)
}

func (m *MoleService) adminJSON(addr, method, path string, body any) error {
	var rdr io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, "http://"+addr+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return errors.New(strings.TrimSpace(string(msg)))
	}
	return nil
}
