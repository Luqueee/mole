// Command mole is a smart local-port forwarder.
//
// It opens a single SSH connection to a remote machine and forwards
// one or more local TCP ports through it to the same port numbers on
// the remote. Optional auto-discover mode probes the remote for
// common dev ports and forwards the ones that respond.
//
// Usage:
//
//	mole up [flags]
//	mole status
//	mole version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/discover"
	"github.com/Luqueee/mole/internal/proxy"
	"github.com/Luqueee/mole/internal/tunnel"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "up":
		os.Exit(runUp(args))
	case "status":
		os.Exit(runStatus(args))
	case "init":
		os.Exit(runInit(args))
	case "version", "-v", "--version":
		fmt.Println("mole", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`mole — local-port forwarder with auto-discover

Usage:
  mole up [flags]
  mole status [flags]
  mole init [flags]
  mole version
  mole help

Commands:
  up       Start the forwarder (foreground)
  status   Query the local admin API
  init     Generate a mole.yaml interactively (or via flags)
  version  Print version and exit

Run 'mole up -h' for up flags, 'mole init -h' for init flags.`)
}

func runUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	var (
		configPath = fs.String("config", "mole.yaml", "path to YAML config (optional)")
		remote     = fs.String("remote", "", "SSH target, e.g. dev@workstation[:port]")
		ports      = fs.String("ports", "", "comma-separated ports to forward")
		autoDisc   = fs.Bool("auto-discover", false, "probe remote for common dev ports")
		adminAddr  = fs.String("admin", "", "admin HTTP address (default 127.0.0.1:9999; empty to disable)")
		logLevel   = fs.String("log-level", "", "debug|info|warn|error")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole up [flags]

Flags:
  -config         path to YAML config (default "mole.yaml")
  -remote         SSH target, e.g. dev@workstation[:port]
  -ports          comma-separated ports to forward (e.g. 3000,5173)
  -auto-discover  probe remote for common dev ports
  -admin          admin HTTP address (empty to disable)
  -log-level      debug|info|warn|error

Either -remote or a config file with 'remote:' is required.`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}

	// CLI overrides.
	if *remote != "" {
		cfg.Remote = *remote
	}
	if *ports != "" {
		cfg.Ports = config.ParsePorts(*ports)
	}
	if *autoDisc {
		cfg.AutoDiscover = true
	}
	if *adminAddr != "" {
		cfg.AdminAddr = *adminAddr
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}

	if cfg.Remote == "" {
		fmt.Fprintln(os.Stderr, "error: -remote is required (or set 'remote:' in config)")
		return 1
	}

	log := newLogger(cfg.LogLevel)

	user, addr, err := tunnel.ParseRemote(cfg.Remote, cfg.SSHPort)
	if err != nil {
		log.Error("invalid remote", "err", err)
		return 1
	}

	log.Info("connecting to remote", "remote", cfg.Remote, "ssh_addr", addr)
	mgr, err := tunnel.New(addr, user, log)
	if err != nil {
		log.Error("ssh connect failed", "err", err)
		return 1
	}
	defer mgr.Close()

	// Determine final port list.
	forwardPorts := cfg.Ports
	if cfg.AutoDiscover {
		log.Info("auto-discovering ports on remote", "candidates", len(cfg.DiscoverPorts))
		found := discover.Probe(mgr, cfg.DiscoverPorts, log)
		sort.Ints(found)
		log.Info("discovery complete", "found", found)
		forwardPorts = config.MergePorts(forwardPorts, found)
	}
	if len(forwardPorts) == 0 {
		log.Error("no ports to forward — use -ports or -auto-discover")
		return 1
	}

	stats := admin.NewStats()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Watch SSH connection and auto-reconnect.
	go mgr.Watch(ctx, 10*time.Second)

	// Start a TCP listener per port.
	var listeners []net.Listener
	for _, p := range forwardPorts {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			log.Warn("could not bind local port, skipping", "port", p, "err", err)
			continue
		}
		listeners = append(listeners, ln)
		remoteAddr := fmt.Sprintf("127.0.0.1:%d", p)
		log.Info("forwarding", "local", ln.Addr().String(), "remote", fmt.Sprintf("%s:%d", cfg.Remote, p))

		hooks := proxy.Hooks{
			OnConnect:    stats.OnConnect,
			OnDisconnect: stats.OnDisconnect,
			OnDialFail:   stats.OnDialFail,
		}
		go func(ln net.Listener, p int) {
			if err := proxy.Serve(ln, mgr, remoteAddr, hooks, log); err != nil {
				log.Warn("proxy terminated", "port", p, "err", err)
			}
		}(ln, p)
	}
	if len(listeners) == 0 {
		log.Error("no listeners could be opened")
		return 1
	}

	// Admin HTTP API.
	var adminSrv *http.Server
	if cfg.AdminAddr != "" {
		info := map[string]any{
			"remote":        cfg.Remote,
			"ports":         forwardPorts,
			"auto_discover": cfg.AutoDiscover,
		}
		srv := admin.New(stats, info)
		adminSrv = &http.Server{
			Addr:              cfg.AdminAddr,
			Handler:           srv.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Info("admin listening", "addr", cfg.AdminAddr)
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("admin server stopped", "err", err)
			}
		}()
	}

	log.Info("mole up. Press Ctrl+C to stop.")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down")

	cancel()
	for _, ln := range listeners {
		_ = ln.Close()
	}
	if adminSrv != nil {
		shutdownCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = adminSrv.Shutdown(shutdownCtx)
	}
	return 0
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("admin", "127.0.0.1:9999", "admin API address")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resp, err := http.Get("http://" + *addr + "/status")
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not reach admin API at", *addr, ":", err)
		fmt.Fprintln(os.Stderr, "is mole running?")
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "admin returned", resp.Status)
		return 1
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read response:", err)
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}