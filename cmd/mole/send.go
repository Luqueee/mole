// mole send — send files to the other machine's inbox.
//
//	mole send [-to SUBFOLDER] [files...]
//
// With no files and a terminal, a picker lists the files of the current
// folder. Run on the machine that runs `mole up`, files go to the remote's
// inbox through the daemon; run on the remote (after `mole inbox receive on`
// on the local machine), they go to the local machine's inbox through the
// port the daemon exposes there.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/transfer"
)

// parseInterspersed parses flags that may come before, between or after the
// positional arguments (`mole send a.txt -to docs`), which the standard
// flag package does not do on its own.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func runSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	to := fs.String("to", "", "sub-folder of the other side's inbox to put the files in")
	direct := fs.Bool("direct", false, "connect to the receiver port directly instead of going through the daemon")
	all := fs.Bool("a", false, "include hidden files in the picker")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole send [flags] [files...]

Sends files to the other machine's inbox (see 'mole inbox'). With no files,
in a terminal, pick them from the current folder.

Flags:
  -to      sub-folder of the other side's inbox to put the files in
  -a       include hidden files in the picker
  -direct  talk to the receiver port directly, not through the daemon
  -config  path to YAML config`)
	}
	paths, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	cfg, err := config.Load(resolveConfigPathReadOnly(*configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}

	if len(paths) == 0 {
		if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
			fmt.Fprintln(os.Stderr, "mole send: no files given (pass file names, or run in a terminal to pick them)")
			return 2
		}
		paths, err = pickFiles(".", *all)
		if err != nil {
			if errors.Is(err, errPickCancelled) {
				return 130
			}
			fmt.Fprintln(os.Stderr, "mole send:", err)
			return 1
		}
	}
	files := make([]transfer.File, 0, len(paths))
	for _, p := range paths {
		f, err := transfer.FileFromPath(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mole send:", err)
			return 1
		}
		files = append(files, f)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	route, err := chooseRoute(cfg, *direct)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole send:", err)
		return 1
	}
	if route == routeDaemon {
		return sendViaDaemon(ctx, cfg, files, *to)
	}
	return sendDirect(ctx, cfg, files, *to)
}

type sendRoute int

const (
	routeDaemon sendRoute = iota
	routeDirect
)

// chooseRoute decides how to reach the other side: through the local daemon
// (this machine runs `mole up`), or straight to the receiver port (this is the
// remote, where the daemon's token and port were installed).
func chooseRoute(cfg *config.Config, direct bool) (sendRoute, error) {
	if direct {
		return routeDirect, nil
	}
	if cfg.AdminAddr != "" && daemonLive(cfg.AdminAddr) {
		return routeDaemon, nil
	}
	if _, err := transfer.ReadToken(transfer.TokenPath(config.StateDir())); err == nil {
		return routeDirect, nil
	}
	if cfg.Remote != "" {
		if cfg.AdminAddr == "" {
			return 0, errors.New("admin_addr is not set in the config, and `mole send` talks to the daemon through it (e.g. admin_addr: 127.0.0.1:9999)")
		}
		return 0, errors.New("mole is not running: start it with `mole up -d`")
	}
	return 0, errors.New("nothing to send to: run this where `mole up` runs, or on the remote after `mole inbox receive on` on the local machine")
}

type progressUI struct {
	on    bool
	name  string
	start time.Time
	last  time.Time
}

func newProgress(name string) *progressUI {
	return &progressUI{on: isTerminal(os.Stderr), name: name, start: time.Now()}
}

func (p *progressUI) update(sent, size int64) {
	if !p.on || time.Since(p.last) < 100*time.Millisecond && sent < size {
		return
	}
	p.last = time.Now()
	pct := 100
	if size > 0 {
		pct = int(sent * 100 / size)
	}
	rate := float64(sent) / max(time.Since(p.start).Seconds(), 0.001)
	fmt.Fprintf(os.Stderr, "\r\x1b[K  %s  %3d%%  %s/s", p.name, pct, humanSize(int64(rate)))
}

func (p *progressUI) clear() {
	if p.on {
		fmt.Fprint(os.Stderr, "\r\x1b[K")
	}
}

type countingReader struct {
	r    io.Reader
	n    int64
	size int64
	ui   *progressUI
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	c.ui.update(c.n, c.size)
	return n, err
}

func printSent(f transfer.File, res transfer.Result, where string, took time.Duration) {
	color := cliColor(os.Stdout)
	dest := filepath.Join(res.Dir, res.Saved)
	if where != "" {
		dest = where + ":" + dest
	}
	fmt.Printf("%s %s  %s  %s %s  %s\n", cGreen("✓", color), f.Name, cDim(humanSize(f.Size), color), cDim("→", color), dest, cDim(took.Round(time.Millisecond).String(), color))
}

func printFailed(name string, err error) {
	color := cliColor(os.Stderr)
	fmt.Fprintf(os.Stderr, "%s %s: %v\n", cRed("✗", color), name, err)
}

// sendViaDaemon uploads each file to the daemon, which relays it to the
// remote. The file's bytes travel in the request, never its path.
func sendViaDaemon(ctx context.Context, cfg *config.Config, files []transfer.File, to string) int {
	failed := 0
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		res, err := uploadToDaemon(ctx, cfg.AdminAddr, f, to)
		if err != nil {
			printFailed(f.Name, err)
			failed++
			continue
		}
		printSent(f, res, cfg.Remote, time.Since(start))
	}
	if failed > 0 || ctx.Err() != nil {
		return 1
	}
	return 0
}

func uploadToDaemon(ctx context.Context, addr string, f transfer.File, to string) (transfer.Result, error) {
	rc, err := f.Open()
	if err != nil {
		return transfer.Result{}, err
	}
	defer rc.Close()
	ui := newProgress(f.Name)
	defer ui.clear()

	q := url.Values{}
	q.Set("name", f.Name)
	q.Set("size", strconv.FormatInt(f.Size, 10))
	q.Set("mode", strconv.FormatUint(uint64(f.Mode), 10))
	q.Set("mtime", strconv.FormatInt(f.MTime, 10))
	if to != "" {
		q.Set("to", to)
	}
	body := &countingReader{r: io.LimitReader(rc, f.Size), size: f.Size, ui: ui}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+addr+"/transfer/send?"+q.Encode(), body)
	if err != nil {
		return transfer.Result{}, err
	}
	req.ContentLength = f.Size
	if f.Size == 0 {
		req.Body = http.NoBody
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return transfer.Result{}, err
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

// sendDirect sends over one connection to the receiver port on this
// machine's loopback, which the daemon's push forward connects to the other
// machine's receiver.
func sendDirect(ctx context.Context, cfg *config.Config, files []transfer.File, to string) int {
	stateDir := config.StateDir()
	token, err := transfer.ReadToken(transfer.TokenPath(stateDir))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mole send: no transfer token here: run `mole inbox receive on` on the machine that runs mole up")
		return 1
	}
	port, err := transfer.ReadPeerPort(transfer.PeerPortPath(stateDir))
	if err != nil {
		port = cfg.Transfer.PortOrDefault()
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mole send: nothing is listening on %s: is mole up on the other machine, with receiving on (`mole inbox receive on`)?\n", addr)
		return 1
	}

	var ui *progressUI
	lastFile := -1
	waiting := time.AfterFunc(1500*time.Millisecond, func() {
		fmt.Fprintln(os.Stderr, "  waiting for the other side to accept the transfer...")
	})
	defer waiting.Stop()
	start := time.Now()
	results, err := transfer.Send(ctx, conn, files, transfer.SendOptions{
		Token: token, To: to,
		Progress: func(i int, sent, size int64) {
			waiting.Stop()
			if i != lastFile {
				if ui != nil {
					ui.clear()
				}
				ui, lastFile = newProgress(files[i].Name), i
			}
			ui.update(sent, size)
		},
	})
	if ui != nil {
		ui.clear()
	}
	for i, res := range results {
		printSent(files[i], res, "", time.Since(start))
	}
	if err != nil {
		name := files[0].Name
		if len(results) < len(files) {
			name = files[len(results)].Name
		}
		printFailed(name, err)
		return 1
	}
	return 0
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
