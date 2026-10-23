// mole inbox — where files sent to this machine land, and who may send.
//
//	mole inbox                    show the inbox, receive state and recent files
//	mole inbox path               print the inbox folder
//	mole inbox set DIR            change the inbox folder
//	mole inbox receive on|off     accept (or stop accepting) files sent from the remote
//	mole inbox mode ask|auto      ask before saving incoming files, or save them at once
//	mole inbox pending            list incoming transfers waiting for an answer
//	mole inbox accept [ID|all]    accept waiting transfers
//	mole inbox reject [ID|all]    reject waiting transfers
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/transfer"
)

func printInboxUsage(w *os.File) {
	fmt.Fprintln(w, `Usage: mole inbox [command]

Where files sent to this machine land, and who may send them.

Commands:
  (none)             show the inbox, whether receiving is on, and recent files
  path               print the inbox folder (cd "$(mole inbox path)")
  set DIR            change the inbox folder
  receive on|off     accept (or stop accepting) files sent from the remote
  mode ask|auto      ask before saving incoming files (default), or save at once
  pending            list incoming transfers waiting for an answer
  accept [ID|all]    accept waiting transfers
  reject [ID|all]    reject waiting transfers

Flags (every command):
  -config  path to YAML config`)
}

func runInbox(args []string) int {
	cmd := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("inbox "+cmd, flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config")
	fs.Usage = func() { printInboxUsage(os.Stderr) }
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	switch cmd {
	case "show", "ls":
		return inboxShow(*configPath)
	case "path":
		cfg, err := config.Load(resolveConfigPathReadOnly(*configPath))
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			return 1
		}
		fmt.Println(cfg.Transfer.InboxDir())
		return 0
	case "set":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "Usage: mole inbox set DIR")
			return 2
		}
		return inboxSet(*configPath, rest[0])
	case "receive":
		if len(rest) != 1 || (rest[0] != "on" && rest[0] != "off") {
			fmt.Fprintln(os.Stderr, "Usage: mole inbox receive on|off")
			return 2
		}
		return inboxReceive(*configPath, rest[0] == "on")
	case "mode":
		if len(rest) != 1 || (rest[0] != "ask" && rest[0] != "auto") {
			fmt.Fprintln(os.Stderr, "Usage: mole inbox mode ask|auto")
			return 2
		}
		return inboxMode(*configPath, rest[0])
	case "pending":
		return inboxPending(*configPath)
	case "accept", "reject":
		return inboxDecide(*configPath, cmd == "accept", rest)
	case "help":
		printInboxUsage(os.Stdout)
		return 0
	}
	fmt.Fprintf(os.Stderr, "mole inbox: unknown command %q\n\n", cmd)
	printInboxUsage(os.Stderr)
	return 2
}

// adminJSON sends one request to the admin API and decodes a JSON answer
// into out (when non-nil).
func adminJSON(method, addr, path string, body any, out any) error {
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, "http://"+addr+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var msg strings.Builder
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		msg.Write(buf[:n])
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(msg.String()))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type transferStatus struct {
	Info struct {
		Transfer struct {
			Receive     bool   `json:"receive"`
			Accept      string `json:"accept"`
			Inbox       string `json:"inbox"`
			RemoteError string `json:"remote_error"`
		} `json:"transfer"`
	} `json:"info"`
}

func fetchTransferStatus(addr string) (transferStatus, error) {
	var st transferStatus
	err := adminJSON(http.MethodGet, addr, "/status", nil, &st)
	return st, err
}

func inboxShow(configPath string) int {
	cfg, err := config.Load(resolveConfigPathReadOnly(configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	color := cliColor(os.Stdout)
	dir := cfg.Transfer.InboxDir()
	fmt.Printf("%s %s\n", cDim("inbox  ", color), dir)

	live := cfg.AdminAddr != "" && daemonLive(cfg.AdminAddr)
	switch {
	case !live:
		state := "off"
		if cfg.Transfer.Receive {
			state = "on (starts with mole up)"
		}
		fmt.Printf("%s %s %s\n", cDim("receive", color), state, cDim("· mole is not running", color))
	default:
		st, err := fetchTransferStatus(cfg.AdminAddr)
		if err != nil {
			fmt.Printf("%s unknown (%v)\n", cDim("receive", color), err)
			break
		}
		t := st.Info.Transfer
		if t.Receive {
			fmt.Printf("%s %s %s\n", cDim("receive", color), cGreen("on", color), cDim("· accept: "+t.Accept, color))
		} else {
			fmt.Printf("%s off %s\n", cDim("receive", color), cDim("· enable with `mole inbox receive on`", color))
		}
		if t.RemoteError != "" {
			fmt.Printf("%s %s\n", cRed("remote ", color), t.RemoteError)
		}
		var pend []transfer.Pending
		if err := adminJSON(http.MethodGet, cfg.AdminAddr, "/transfer/pending", nil, &pend); err == nil && len(pend) > 0 {
			fmt.Printf("%s %d waiting: `mole inbox pending`\n", cDim("pending", color), len(pend))
		}
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		fmt.Printf("\n%s\n", cDim("(empty: the folder is created on the first transfer)", color))
		return 0
	}
	type file struct {
		name string
		size int64
		mod  time.Time
	}
	var files []file
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() && !info.IsDir() {
			continue
		}
		files = append(files, file{e.Name(), info.Size(), info.ModTime()})
	}
	if len(files) == 0 {
		fmt.Printf("\n%s\n", cDim("(empty)", color))
		return 0
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > 15 {
		files = files[:15]
	}
	fmt.Println()
	for _, f := range files {
		fmt.Printf("  %-40s %10s  %s\n", f.name, humanSize(f.size), cDim(ago(f.mod), color))
	}
	return 0
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// loadTransferForWrite loads the config to edit and returns its path. With
// no config anywhere it creates the user-global one: the inbox is a property
// of the machine, and `mole recv` on the remote runs from the home folder, so
// a ./mole.yaml written from whatever folder this ran in would never be seen.
func loadTransferForWrite(configPath string) (*config.Config, string, error) {
	path := configPath
	if path == "" {
		path = config.Find()
	}
	if path == "" {
		path = config.GlobalPath()
	}
	cfg, err := config.Load(path)
	return cfg, path, err
}

// restartReceiver makes a running daemon pick up changed transfer settings.
func restartReceiver(cfg *config.Config) {
	if cfg.AdminAddr == "" || !daemonLive(cfg.AdminAddr) {
		return
	}
	st, err := fetchTransferStatus(cfg.AdminAddr)
	if err != nil || !st.Info.Transfer.Receive {
		return
	}
	_ = adminJSON(http.MethodPut, cfg.AdminAddr, "/transfer/receive", map[string]bool{"on": false}, nil)
	if err := adminJSON(http.MethodPut, cfg.AdminAddr, "/transfer/receive", map[string]bool{"on": true}, nil); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not restart the receiver (%v); run `mole restart`\n", err)
		return
	}
	fmt.Println("live: the receiver picked up the change")
}

func inboxSet(configPath, dir string) int {
	cfg, path, err := loadTransferForWrite(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if !strings.HasPrefix(dir, "~") {
		abs, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mole inbox:", err)
			return 1
		}
		dir = abs
	}
	cfg.Transfer.Inbox = dir
	if err := cfg.Transfer.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "mole inbox:", err)
		return 2
	}
	if err := os.MkdirAll(cfg.Transfer.InboxDir(), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mole inbox:", err)
		return 1
	}
	if err := config.Save(path, cfg, "transfer"); err != nil {
		fmt.Fprintf(os.Stderr, "mole inbox: write %s: %v\n", path, err)
		return 1
	}
	fmt.Printf("inbox is now %s (saved in %s)\n", cfg.Transfer.InboxDir(), path)
	restartReceiver(cfg)
	return 0
}

func inboxMode(configPath, mode string) int {
	cfg, path, err := loadTransferForWrite(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	cfg.Transfer.Accept = mode
	if mode == "ask" {
		cfg.Transfer.Accept = "" // the default
	}
	if err := config.Save(path, cfg, "transfer"); err != nil {
		fmt.Fprintf(os.Stderr, "mole inbox: write %s: %v\n", path, err)
		return 1
	}
	fmt.Printf("incoming transfers: %s (saved in %s)\n", mode, path)
	restartReceiver(cfg)
	return 0
}

func inboxReceive(configPath string, on bool) int {
	cfg, path, err := loadTransferForWrite(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	cfg.Transfer.Receive = on
	if err := config.Save(path, cfg, "transfer"); err != nil {
		fmt.Fprintf(os.Stderr, "mole inbox: write %s: %v\n", path, err)
		return 1
	}
	fmt.Printf("receive %s (saved in %s)\n", map[bool]string{true: "on", false: "off"}[on], path)
	if cfg.AdminAddr == "" || !daemonLive(cfg.AdminAddr) {
		fmt.Println("note: mole up is not running; it will apply this on start")
		return 0
	}
	if err := adminJSON(http.MethodPut, cfg.AdminAddr, "/transfer/receive", map[string]bool{"on": on}, nil); err != nil {
		fmt.Fprintf(os.Stderr, "warning: live change failed (%v); saved, mole up retries it after the next reconnect\n", err)
		return 1
	}
	if on {
		fmt.Printf("live: files sent from the remote with `mole send` land in %s\n", cfg.Transfer.InboxDir())
	} else {
		fmt.Println("live: stopped receiving")
	}
	return 0
}

func fetchPending(cfg *config.Config) ([]transfer.Pending, error) {
	if cfg.AdminAddr == "" || !daemonLive(cfg.AdminAddr) {
		return nil, fmt.Errorf("mole is not running")
	}
	var pend []transfer.Pending
	err := adminJSON(http.MethodGet, cfg.AdminAddr, "/transfer/pending", nil, &pend)
	return pend, err
}

func describePending(p transfer.Pending) string {
	names := make([]string, 0, len(p.Files))
	for _, f := range p.Files {
		names = append(names, f.Name)
	}
	list := strings.Join(names, ", ")
	if len(list) > 60 {
		list = list[:57] + "..."
	}
	return fmt.Sprintf("%d  from %s  %d file(s), %s  %s", p.ID, p.Peer, len(p.Files), humanSize(p.Total), list)
}

func inboxPending(configPath string) int {
	cfg, err := config.Load(resolveConfigPathReadOnly(configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	pend, err := fetchPending(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole inbox:", err)
		return 1
	}
	if len(pend) == 0 {
		fmt.Println("nothing waiting")
		return 0
	}
	for _, p := range pend {
		fmt.Println(describePending(p))
	}
	return 0
}

func inboxDecide(configPath string, accept bool, args []string) int {
	cfg, err := config.Load(resolveConfigPathReadOnly(configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	pend, err := fetchPending(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole inbox:", err)
		return 1
	}
	verb := map[bool]string{true: "accepted", false: "rejected"}[accept]
	var ids []uint64
	switch {
	case len(args) == 1 && args[0] == "all":
		for _, p := range pend {
			ids = append(ids, p.ID)
		}
	case len(args) == 1:
		id, err := strconv.ParseUint(args[0], 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mole inbox: invalid id %q\n", args[0])
			return 2
		}
		ids = []uint64{id}
	case len(args) == 0 && len(pend) == 1:
		ids = []uint64{pend[0].ID}
	case len(args) == 0 && len(pend) == 0:
		fmt.Println("nothing waiting")
		return 0
	default:
		fmt.Fprintln(os.Stderr, "mole inbox: several transfers are waiting; name one (or `all`):")
		for _, p := range pend {
			fmt.Fprintln(os.Stderr, "  "+describePending(p))
		}
		return 2
	}
	failed := 0
	for _, id := range ids {
		path := "/transfer/pending/" + strconv.FormatUint(id, 10)
		if err := adminJSON(http.MethodPost, cfg.AdminAddr, path, map[string]bool{"accept": accept}, nil); err != nil {
			fmt.Fprintf(os.Stderr, "mole inbox: %d: %v\n", id, err)
			failed++
			continue
		}
		fmt.Printf("%s %d\n", verb, id)
	}
	if failed > 0 {
		return 1
	}
	return 0
}
