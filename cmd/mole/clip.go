// mole clip — share clipboard images between a Mac and a remote
// host (typically an LXC) over a private WireGuard or Tailscale link.
//
// Two sub-modes:
//
//	mole clip serve [-listen ADDR] [-watch] [-interval DUR]
//	    Runs on the Mac. Exposes an HTTP endpoint that serves the
//	    most recently captured clipboard image. With -watch, also
//	    polls the macOS pasteboard and pushes every new image to
//	    the endpoint (no external tool needed).
//
//	mole clip pull [-url URL]
//	    Runs on the remote. Fetches the latest image and prints
//	    the path to stdout, where the shell / Claude Code reads it.
//
// The two endpoints discover each other through mole.yaml
// (clip_url, clip_listen, clip_interval_ms); the watch/push loop is
// intra-process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Luqueee/mole/internal/clip"
	"github.com/Luqueee/mole/internal/config"
)

func runClip(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mole clip <serve|pull> [flags]")
		fmt.Fprintln(os.Stderr, "run 'mole clip -h' for the full help")
		return 2
	}
	switch args[0] {
	case "serve":
		return runClipServe(args[1:])
	case "pull":
		return runClipPull(args[1:])
	case "-h", "--help", "help":
		printClipUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mole clip: unknown subcommand: %s\n\n", args[0])
		printClipUsage(os.Stderr)
		return 2
	}
}

func printClipUsage(w *os.File) {
	color := cliColor(w)
	fmt.Fprintf(w, "%s\n\n", cBold("mole clip — share clipboard images over a private WireGuard or Tailscale link", color))
	fmt.Fprintf(w, "  %s\n", cBold("USAGE", color))
	fmt.Fprintf(w, "    mole clip %s\n", cDim("<serve|pull> [flags]", color))
	fmt.Println()
	fmt.Fprintf(w, "  %s\n", cBold("SUBCOMMANDS", color))
	fmt.Fprintf(w, "    %s  %s\n", cGreen("serve", color), "Run on the Mac; expose the clipboard over HTTP (and watch it).")
	fmt.Fprintf(w, "    %s  %s\n", cGreen("pull", color), "Run on the remote; fetch the latest image and print its path.")
	fmt.Println()
	fmt.Fprintf(w, "  %s\n", cBold("NOTES", color))
	fmt.Fprintf(w, "    %s\n", cDim("  serve binds -listen (default "+config.DefaultClipListen+"). The endpoint has no auth;", color))
	fmt.Fprintf(w, "    %s\n", cDim("  use a private Tailscale/WireGuard address when remote access is required.", color))
	fmt.Fprintf(w, "    %s\n", cDim("  pull prints the image path on stdout and exits. Exit code 3 means", color))
	fmt.Fprintf(w, "    %s\n", cDim("  'no image on the server yet'.", color))
}

// runClipServe starts an HTTP server that serves the last image
// pushed to it and, if -watch is set, polls the macOS clipboard in
// the background.
func runClipServe(args []string) int {
	fs := flag.NewFlagSet("clip serve", flag.ExitOnError)
	var (
		configPath = fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
		listen     = fs.String("listen", config.DefaultClipListen, "address to bind the clip HTTP server")
		watch      = fs.Bool("watch", true, "poll the macOS clipboard and auto-push new images")
		interval   = fs.Duration("interval", 500*time.Millisecond, "clipboard poll cadence (only used with -watch)")
		logLevel   = fs.String("log-level", "", "debug|info|warn|error")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole clip serve [flags]

Runs on the Mac. Exposes the clipboard over HTTP and (optionally)
polls the macOS pasteboard to keep the latest image cached.

Flags:
  -config       path to YAML config (default: ./mole.yaml, then user-global)
  -listen       bind address (default 127.0.0.1:7777)
  -watch        poll the macOS clipboard and push new images (default true)
  -interval     clipboard poll cadence (default 500ms)
  -log-level    debug|info|warn|error`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadClipConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}
	// CLI flags win over YAML, matching runUp's behaviour. We use
	// isFlagSet to detect which flags the user actually passed, so
	// a bare `mole clip serve` on a host with a stale config still
	// binds the documented default.

	// Apply config defaults only for flags the user did NOT set.
	if !isFlagSet(fs, "listen") && cfg.ClipListen != "" {
		*listen = cfg.ClipListen
	}
	if !isFlagSet(fs, "interval") && cfg.ClipIntervalMs > 0 {
		*interval = time.Duration(cfg.ClipIntervalMs) * time.Millisecond
	}
	if !isFlagSet(fs, "log-level") && *logLevel == "" {
		*logLevel = cfg.LogLevel
	}
	log := newLogger(*logLevel)
	if isBroadClipBind(*listen) {
		log.Warn("clip server has no authentication and is listening on all interfaces", "addr", *listen, "hint", "use loopback or a private Tailscale/WireGuard address")
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("clip serve: listen failed", "addr", *listen, "err", err)
		return 1
	}
	defer ln.Close()

	srv := &http.Server{
		Handler:           clip.New(log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("clip server up", "addr", ln.Addr().String(), "watch", *watch, "interval", interval.String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("clip server stopped", "err", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Watcher only exists on Darwin. On other OSes -watch is a
	// silent no-op (the binary still serves HTTP fine, but the
	// pasteboard poll never starts). Documented in -h.
	if *watch {
		w := clip.NewWatcher(log)
		if err := w.Run(ctx, "http://"+ln.Addr().String(), *interval); err != nil && !errors.Is(err, context.Canceled) {
			// Non-fatal: the server is still up; the user can
			// re-enable later or push images some other way.
			if errors.Is(err, clip.ErrUnavailable) {
				log.Info("clip watcher: not supported on this OS; serving HTTP only")
			} else {
				log.Warn("clip watcher exited", "err", err)
			}
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("clip serve: shutting down")
	shutdownCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	_ = srv.Shutdown(shutdownCtx)
	return 0
}

func isBroadClipBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// runClipPull fetches the latest image from the clip server and
// prints its path to stdout. Exits 0 on success, 3 on
// ErrNoImage, 1 on any other error.
func runClipPull(args []string) int {
	fs := flag.NewFlagSet("clip pull", flag.ExitOnError)
	var (
		configPath = fs.String("config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
		url        = fs.String("url", "", "clip server URL (overrides mole.yaml clip_url)")
		logLevel   = fs.String("log-level", "", "debug|info|warn|error")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: mole clip pull [flags]

Runs on the remote. Fetches the latest image and prints the path
on stdout. Exit code 3 means "no image on the server yet" — Claude
Code can treat that as "nothing to paste, try again later".

Flags:
  -config       path to YAML config
  -url          clip server URL (e.g. http://10.0.0.1:7777)
  -log-level    debug|info|warn|error`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadClipConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}
	if *url == "" {
		*url = cfg.ClipURL
	}
	if *url == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required (or set 'clip_url:' in config)")
		return 1
	}
	if *logLevel == "" {
		*logLevel = cfg.LogLevel
	}
	log := newLogger(*logLevel)

	c := clip.NewClient(strings.TrimRight(*url, "/"), log)
	path, err := c.Pull(context.Background())
	if errors.Is(err, clip.ErrNoImage) {
		fmt.Fprintln(os.Stderr, "no image on the server yet")
		return 3
	}
	if err != nil {
		log.Error("clip pull failed", "err", err)
		return 1
	}
	fmt.Println(path)
	return 0
}

// loadClipConfig resolves a config the same way runUp does: explicit
// path if given, else the standard search order. Wrapping it here
// keeps the clip subcommand free of config-loading boilerplate.
func loadClipConfig(path string) (*config.Config, error) {
	resolved := path
	if resolved == "" {
		resolved = config.Find()
	}
	return config.Load(resolved)
}

// isFlagSet reports whether the user passed the named flag on the
// command line. Used to keep CLI flags authoritative over YAML.
func isFlagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
