// mole agent — the control channel other nodes use to manage this one. It is
// plumbing: `mole -n NODE ...` and `mole node ls` start it over SSH.
//
//	mole agent --stdio
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/Luqueee/mole/internal/agent"
	"github.com/Luqueee/mole/internal/config"
)

// nodeAgent answers the agent protocol from this node's config and daemon.
type nodeAgent struct{ configPath string }

func (a nodeAgent) Hello() agent.Hello {
	host, _ := os.Hostname()
	return agent.Hello{
		Agent: agent.ProtocolVersion, Version: version, Build: buildLabel(), OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: host,
		Caps: []string{agent.OpHello, agent.OpStatus},
	}
}

func (a nodeAgent) Status() (agent.Status, error) {
	path := resolveConfigPathReadOnly(a.configPath)
	cfg, err := config.Load(path)
	if err != nil {
		return agent.Status{}, fmt.Errorf("config: %w", err)
	}
	st := agent.Status{
		Remote: cfg.Remote, AdminAddr: cfg.AdminAddr, ConfigPath: path,
		Inbox: cfg.Transfer.InboxDir(), Nodes: len(cfg.Nodes),
	}
	if cfg.AdminAddr == "" || !daemonLive(cfg.AdminAddr) {
		return st, nil
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + cfg.AdminAddr + "/status")
	if err != nil {
		return st, nil
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&raw) == nil {
		st.Running, st.Admin = true, raw
	}
	return st, nil
}

func runAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	stdio := fs.Bool("stdio", false, "serve the agent protocol on stdin/stdout")
	configPath := fs.String("config", "", "path to YAML config")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: mole agent --stdio\n\nPlumbing for `mole -n` and `mole node`: other nodes run this over SSH.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*stdio {
		fs.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.Serve(ctx, stdioConn{os.Stdin, os.Stdout}, nodeAgent{configPath: *configPath}); err != nil {
		fmt.Fprintln(os.Stderr, "mole agent:", err)
		return 1
	}
	return 0
}
