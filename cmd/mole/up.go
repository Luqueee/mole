// The `mole up` server: one run of the forwarder, from start to shutdown.
//
// upServer.run goes through these phases; shutdown undoes them in reverse.
//
//  1. claim    become the user's one mole server (instance lock + pidfile).
//  2. prepare  load the config, apply the flags, check what must be right
//     before anything starts.
//  3. admin    start the admin API: /health and /status (info.state
//     "connecting") answer at once; the endpoints that need the tunnel
//     answer 503 until it is up.
//  4. connect  open the SSH connection, retrying while the network comes up.
//  5. start    push forwards, file transfer, the connection watch, port
//     forwards, named routes and the DNS stub; then the admin API switches
//     to the full handler (info.state "connected") and discovery starts.
//  6. serve    until a stop is requested (SIGINT/SIGTERM, `mole down`).
//  7. shutdown stop the admin API, then every started component, newest
//     first; the instance claim is released last.
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
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Luqueee/mole/internal/admin"
	"github.com/Luqueee/mole/internal/config"
	"github.com/Luqueee/mole/internal/meshdns"
	"github.com/Luqueee/mole/internal/proxy"
	"github.com/Luqueee/mole/internal/routes"
	"github.com/Luqueee/mole/internal/tunnel"
)

// upFlags are the flags of `mole up`.
type upFlags struct {
	configPath   string
	remote       string
	ports        string
	autoDiscover bool
	adminAddr    string
	logLevel     string
	detach       bool // -d
	detachLong   bool // -detach
	insecure     bool
}

// detached reports whether -d or -detach was given.
func (f upFlags) detached() bool { return f.detach || f.detachLong }

// parseUpFlags parses the arguments of `mole up`, printing errors and the
// usage to output. A flag.ErrHelp error means -h was asked for.
func parseUpFlags(args []string, output io.Writer) (upFlags, error) {
	var f upFlags
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&f.configPath, "config", "", "path to YAML config (default: ./mole.yaml, then user-global)")
	fs.StringVar(&f.remote, "remote", "", "SSH target, e.g. dev@workstation[:port]")
	fs.StringVar(&f.ports, "ports", "", "comma-separated ports to forward")
	fs.BoolVar(&f.autoDiscover, "auto-discover", false, "probe remote for common dev ports")
	fs.StringVar(&f.adminAddr, "admin", "", "admin HTTP address, loopback only; overrides admin_addr (the API is off when neither sets one)")
	fs.StringVar(&f.logLevel, "log-level", "", "debug|info|warn|error")
	fs.BoolVar(&f.detach, "d", false, "run in the background (daemon)")
	fs.BoolVar(&f.detachLong, "detach", false, "alias for -d")
	fs.BoolVar(&f.insecure, "insecure", false, "disable SSH host key verification (UNSAFE; dev only)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `Usage: mole up [flags]

Flags:
  -config         path to YAML config (default: ./mole.yaml, then user-global)
  -remote         SSH target, e.g. dev@workstation[:port]
  -ports          comma-separated ports to forward (e.g. 3000,5173)
  -auto-discover  probe remote for common dev ports
  -admin          admin HTTP address, loopback only; overrides admin_addr
  -log-level      debug|info|warn|error
  -insecure       disable SSH host key verification (UNSAFE; dev only)
  -d, -detach     run in the background (daemon); stop with 'mole down'

Either -remote or a config file with 'remote:' is required.`)
	}
	err := fs.Parse(args)
	return f, err
}

// config returns the configuration `mole up` runs with and the file it came
// from ("" when there is none). Without -config the standard locations are
// searched (project-local ./mole.yaml, then user-global), so a config written
// by `mole init -global` is picked up automatically. The flags override the
// file, which overrides the defaults. An error is the line to print; each one
// ends the run with exit status 1.
func (f upFlags) config() (*config.Config, string, error) {
	path := f.configPath
	if path == "" {
		path = config.Find()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", fmt.Errorf("config error: %w", err)
	}

	// CLI overrides.
	if f.remote != "" {
		cfg.Remote = f.remote
	}
	if f.ports != "" {
		cfg.Ports = config.ParsePorts(f.ports)
	}
	if f.autoDiscover {
		cfg.AutoDiscover = true
	}
	if f.adminAddr != "" {
		cfg.AdminAddr = f.adminAddr
	}
	if f.logLevel != "" {
		cfg.LogLevel = f.logLevel
	}
	if f.insecure {
		cfg.Insecure = true
	}

	if cfg.Remote == "" {
		return nil, "", errors.New("error: -remote is required (or set 'remote:' in config)")
	}
	// The admin API has no authentication and can push local ports to the
	// remote and accept files, so it only listens on loopback. Checked before
	// connecting so a bad admin_addr fails at once, not after the SSH dial.
	if cfg.AdminAddr != "" {
		if err := admin.CheckListenAddr(cfg.AdminAddr); err != nil {
			return nil, "", fmt.Errorf("config error: %w", err)
		}
	}
	return cfg, path, nil
}

// The connection state /status reports under info.state.
const (
	stateConnecting = "connecting" // the first SSH connection is not up yet
	stateConnected  = "connected"  // everything is up (reconnects happen underneath)
)

// upServer is one run of `mole up`.
type upServer struct {
	flags upFlags

	// Set by newUpServer; tests replace them.
	stderr          io.Writer // log output and error messages
	waitForInstance bool      // wait for a running mole to stop instead of failing (the login service)
	backoff         backoff   // retries of the first connection
	dial            func(rem tunnel.Remote, insecure bool, log *slog.Logger) (*tunnel.Manager, error)

	// Set by prepare.
	cfg      *config.Config
	cfgPath  string // the config file in use; "" when there is none
	log      *slog.Logger
	remote   tunnel.Remote
	stats    *admin.Stats
	routeCtl *routeManager // nil without domains

	ctx    context.Context // lifetime of the components; cancelled first at shutdown
	cancel context.CancelFunc
	admin  *adminAPI // nil when admin_addr is unset or could not be listened on
	undo   []func()  // stop the started components; run newest first
}

func newUpServer(f upFlags) *upServer {
	return &upServer{
		flags:           f,
		stderr:          os.Stderr,
		waitForInstance: os.Getenv(serviceEnv) != "",
		backoff:         startupBackoff,
		dial:            tunnel.New,
	}
}

// run runs the server until stop is done and returns the exit status.
func (s *upServer) run(stop context.Context) int {
	// Detached, in a terminal or under a service manager, one mole server
	// runs per user: it holds the instance lock for the whole run and owns
	// the pidfile (removed on exit). Under the login service it waits for a
	// running mole to stop instead of failing into a restart loop. The
	// config is read after the claim, so a server that had to wait starts
	// with the config as it is then, edits made meanwhile included.
	inst, err := claimInstance(stop, s.waitForInstance)
	if err != nil {
		if stop.Err() != nil {
			return 0
		}
		fmt.Fprintln(s.stderr, err)
		return 1
	}
	defer inst.release()

	if !s.prepare() {
		return 1
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.shutdown()

	s.startAdmin()

	s.log.Info("connecting to remote", "remote", s.cfg.Remote, "ssh_addr", s.remote.Addr, "user", s.remote.User)
	// Started at login, mole may run before the network is up: transient
	// failures are retried until the remote answers or we are stopped. The
	// admin API answers meanwhile, so `mole status` and the desktop app see a
	// mole that is connecting, not one that is not running.
	mgr, err := connectWhenReachable(stop, s.log, s.backoff, func() (*tunnel.Manager, error) {
		return s.dial(s.remote, s.cfg.Insecure, s.log)
	})
	if err != nil {
		if stop.Err() != nil {
			s.log.Info("shutting down")
			return 0
		}
		s.log.Error("ssh connect failed", "err", err)
		return 1
	}
	if !s.start(mgr) {
		return 1
	}

	s.log.Info("mole up. Press Ctrl+C to stop.")
	<-stop.Done()
	s.log.Info("shutting down")
	return 0
}

// prepare loads and checks the config and sets up what needs no remote: the
// logger, the SSH target, the stats and the route table. It reports a
// problem and returns false when the run cannot go on.
func (s *upServer) prepare() bool {
	cfg, path, err := s.flags.config()
	if err != nil {
		fmt.Fprintln(s.stderr, err)
		return false
	}
	s.cfg, s.cfgPath = cfg, path
	s.log = newLoggerTo(s.stderr, cfg.LogLevel)

	rem, err := tunnel.ResolveRemote(cfg.Remote, cfg.SSHPort)
	if err != nil {
		s.log.Error("invalid remote", "err", err)
		return false
	}
	s.remote = rem
	s.stats = admin.NewStats()
	if len(cfg.Domains) > 0 {
		s.routeCtl = &routeManager{
			table:   routes.NewTable(cfg.Domains, cfg.Routes),
			hasNode: func(name string) bool { return nodeConfigured(path, name) },
		}
	}
	return true
}

// onShutdown registers a shutdown step; steps run newest first.
func (s *upServer) onShutdown(fn func()) { s.undo = append(s.undo, fn) }

// shutdown ends the run: first the components' context (the connection
// watch, discovery), then the admin API, so no request acts on a component
// being stopped, then every started component, newest first.
func (s *upServer) shutdown() {
	s.cancel()
	s.admin.close()
	for i := len(s.undo) - 1; i >= 0; i-- {
		s.undo[i]()
	}
	s.undo = nil
}

// startAdmin starts the admin HTTP API (admin_addr was checked to be
// loopback). It serves the connecting API until start switches it to the
// full one. An address it cannot listen on is logged, not fatal.
func (s *upServer) startAdmin() {
	if s.cfg.AdminAddr == "" {
		return
	}
	ln, err := net.Listen("tcp", s.cfg.AdminAddr)
	if err != nil {
		s.log.Warn("admin server stopped", "err", err)
		return
	}
	a := &adminAPI{ln: ln}
	a.serve(s.connectingAPI().Handler())
	a.srv = &http.Server{Handler: a, ReadHeaderTimeout: 5 * time.Second}
	s.admin = a
	s.log.Info("admin listening", "addr", ln.Addr().String())
	go func() {
		if err := a.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("admin server stopped", "err", err)
		}
	}()
}

// adminInfo is the part of /status info the daemon supplies itself.
func (s *upServer) adminInfo(state string) map[string]any {
	return map[string]any{
		"remote":        s.cfg.Remote,
		"auto_discover": s.cfg.AutoDiscover,
		"domains":       s.cfg.Domains,
		"state":         state,
	}
}

// connectingAPI is the admin API served while the first SSH connection is
// being made. Its /status has the shape of the connected one, so clients
// need no special case: info.state "connecting", no ports forwarded yet, the
// configured pushes in state "connecting", the transfer settings with
// receive off. The endpoints that need the tunnel answer 503.
func (s *upServer) connectingAPI() *admin.Server {
	cfg := s.cfg
	push := make([]admin.PushInfo, 0, len(cfg.Push))
	for _, r := range cfg.Push {
		push = append(push, admin.PushInfo{Local: r.Local, Remote: r.RemotePort(), Bind: r.BindAddr(), State: stateConnecting})
	}
	sort.Slice(push, func(i, j int) bool { return push[i].Local < push[j].Local })
	info := s.adminInfo(stateConnecting)
	info["ports"] = []int{}
	info["push"] = push
	info["transfer"] = transferInfo(cfg.Transfer, false, "")
	srv := admin.New(s.stats, info).WithUnavailable(fmt.Sprintf(
		"mole is still connecting to %s: this works once it is connected (`mole status` shows \"state\":\"connected\"; see `mole logs`)", cfg.Remote))
	if s.routeCtl != nil {
		srv = srv.WithRouteController(s.routeCtl)
	}
	return srv
}

// start brings up everything that works through the SSH connection, then
// switches the admin API to the full handler and starts discovery. It
// returns false when the run cannot go on.
func (s *upServer) start(mgr *tunnel.Manager) bool {
	cfg, log, stats := s.cfg, s.log, s.stats
	s.onShutdown(func() { _ = mgr.Close() })

	// Push forwards (local -> remote). Remote listeners die with the SSH
	// connection, so the reconnect hook must be registered before Watch
	// starts, or a reconnect during startup would be missed.
	pushFwd := newPushForwarder(mgr, log, stats.Hooks(proxy.DirPush))
	s.onShutdown(pushFwd.closeAll)
	mgr.OnReconnect(pushFwd.reopenAll)

	// File transfer: sending to the remote's inbox always works; receiving
	// from the remote needs transfer.receive (see `mole inbox receive on`).
	hub := newTransferHub(log, cfg.Remote, s.cfgPath, cfg.Transfer, mgr, pushFwd, config.StateDir())
	s.onShutdown(hub.close)
	mgr.OnReconnect(hub.onReconnect)

	// Watch SSH connection and auto-reconnect.
	go mgr.Watch(s.ctx, 10*time.Second)

	// The forwarder owns the per-port listeners and can add new ones at
	// runtime, which is what makes periodic auto-discovery work. The ports
	// configured explicitly are pinned: auto-discovery's pruning never
	// closes them, even if the remote service is momentarily down.
	exclude := s.excludeSet()
	pinned := make(map[int]bool, len(cfg.Ports))
	for _, p := range cfg.Ports {
		pinned[p] = true
	}
	fwd := &forwarder{
		active:      make(map[int]net.Listener),
		failed:      make(map[int]bool),
		pinned:      pinned,
		mgr:         mgr,
		remoteLabel: cfg.Remote,
		log:         log,
		exclude:     exclude,
		hooks:       stats.Hooks(proxy.DirPull),
	}
	s.onShutdown(fwd.closeAll)
	// A port that is pushed to the same number on the remote is never also
	// pulled: the two would feed each other forever.
	fwd.loops = pushFwd.loopsPort
	pushFwd.onLoop = func(port int) { _ = fwd.RemoveDiscover(port) }

	pushFwd.start(cfg.Push)
	hub.enableAtStart()

	if !s.startRoutes(mgr) {
		return false
	}

	// Forward the explicitly-configured (pinned) ports up front.
	for _, p := range cfg.Ports {
		_ = fwd.ensure(p)
	}

	// Everything the admin API controls is up: serve all of it. The
	// controllers are safe for concurrent use, so it may go live before the
	// first discovery sweep.
	if s.admin != nil {
		srv := admin.New(stats, s.adminInfo(stateConnected)).
			WithPorts(fwd.ports).
			WithPortController(fwd).
			WithPushController(pushFwd).
			WithTransferController(hub)
		if s.routeCtl != nil {
			srv = srv.WithRouteController(s.routeCtl)
		}
		s.admin.serve(srv.Handler())
	}

	if cfg.AutoDiscover {
		s.startDiscovery(fwd, mgr, exclude)
	}

	// An empty set is fine: ports and pushes can be added at runtime
	// (mole ports add, mole push, the desktop app).
	if !cfg.AutoDiscover && len(fwd.ports()) == 0 && len(cfg.Push) == 0 && len(cfg.Domains) == 0 {
		log.Info("nothing to forward yet; add ports or pushes at runtime (`mole ports add`, `mole push`)")
	}
	return true
}

// excludeSet is the set of ports never auto-forwarded. The forwarder's
// live-add path (AddDiscover, behind the admin API) consults it as well, so
// it exists even when auto-discover is off. It is complete before the first
// discovery sweep.
func (s *upServer) excludeSet() map[int]bool {
	exclude := make(map[int]bool, len(s.cfg.ExcludePorts)+1)
	for _, p := range s.cfg.ExcludePorts {
		exclude[p] = true
	}
	// A remote service on the admin API's own port must not be forwarded
	// onto it. The admin API normally holds that port already (it starts
	// before the connection), so the forward would only fail to bind; but
	// when it could not listen, the forward would win the port and every
	// client of the admin API (the desktop app, `mole status`) would be
	// talking to the remote service instead.
	if p, ok := loopbackPort(s.cfg.AdminAddr); ok {
		exclude[p] = true
	}
	return exclude
}

// startRoutes starts the Host-routing proxy for named routes and, for
// domains outside .localhost, the DNS stub. It returns false when the DNS
// stub cannot listen.
func (s *upServer) startRoutes(mgr *tunnel.Manager) bool {
	cfg, log := s.cfg, s.log
	// The Host-routing proxy also works without system DNS under .localhost.
	if s.routeCtl == nil {
		if len(cfg.Routes) > 0 {
			log.Warn("routes: ignored, no domains: configured (`mole domain set NAME`)")
		}
		return true
	}
	table := s.routeCtl.table
	// Routes to other nodes go through a tunnel of their own, opened the
	// first time one is used; the primary remote reuses mgr.
	var pool *routes.Pool
	pool = routes.NewPool(mgr, func(name string) (routes.NodeConn, error) {
		return openNodeTunnel(pool.Context(), s.cfgPath, name, log)
	}, log)
	s.onShutdown(pool.Close)
	routePort := cfg.RoutePortOrDefault()
	if rl, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(routePort))); err != nil {
		log.Warn("route proxy bind failed; named routes will not answer", "port", routePort, "err", err)
	} else {
		s.onShutdown(func() { _ = rl.Close() })
		go func() {
			if err := routes.New(table, pool, s.stats.Hooks(proxy.DirPull), log).Serve(rl); err != nil {
				log.Warn("route proxy terminated", "err", err)
			}
		}()
		log.Info("route proxy listening", "addr", rl.Addr().String(), "routes", len(cfg.Routes))
	}

	if needsDNSStub(cfg.Domains) {
		mesh := meshdns.New(cfg.Domains, cfg.DomainPortsOrDefault(), mgr, s.stats.Hooks(proxy.DirPull), log)
		s.onShutdown(mesh.Close)
		mesh.SetRoutes(table)
		stubAddr := meshdns.StubListenAddr()
		pc, err := net.ListenPacket("udp", stubAddr)
		if err != nil {
			log.Error("dns stub bind failed", "addr", stubAddr, "err", err)
			return false
		}
		s.onShutdown(func() { _ = pc.Close() })
		tl, err := net.Listen("tcp", stubAddr)
		if err != nil {
			log.Error("dns stub bind failed", "addr", stubAddr, "err", err)
			return false
		}
		s.onShutdown(func() { _ = tl.Close() })
		go mesh.ServeUDP(pc)
		go mesh.ServeTCP(tl)
		log.Info("dns stub listening", "addr", stubAddr, "domains", cfg.Domains, "ports", cfg.DomainPortsOrDefault())
	}
	return true
}

// startDiscovery runs the first discovery sweep, then keeps re-discovering
// so dev servers that come up after launch are forwarded automatically — no
// need to restart mole when you start your dev server.
func (s *upServer) startDiscovery(fwd *forwarder, mgr *tunnel.Manager, exclude map[int]bool) {
	// discover_range (validated by config.Load) narrows which enumerated
	// listeners are auto-forwarded; unset, all of them are.
	discoverRange := s.cfg.DiscoverPortRange()
	if r := discoverRange.String(); r != "" {
		s.log.Info("auto-discover limited to a port range", "discover_range", r)
	}
	ctx, candidates, inRange, log := s.ctx, s.cfg.DiscoverPorts, discoverRange.Contains, s.log
	discoverInto(ctx, fwd, mgr, candidates, exclude, inRange, log)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				discoverInto(ctx, fwd, mgr, candidates, exclude, inRange, log)
			}
		}
	}()
}

// adminAPI is the daemon's admin HTTP server. It listens from the start of
// the run and serves one handler at a time, swapped atomically: the
// connecting API, then the full one. Both come from admin.Server.Handler, so
// the browser guard and the Content-Type checks hold in every state.
type adminAPI struct {
	ln  net.Listener
	srv *http.Server
	h   atomic.Pointer[handlerRef]
}

type handlerRef struct{ http.Handler }

func (a *adminAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.h.Load().ServeHTTP(w, r) }

// serve makes h answer every request from now on.
func (a *adminAPI) serve(h http.Handler) { a.h.Store(&handlerRef{h}) }

// close stops the server, giving requests in flight a moment to finish.
func (a *adminAPI) close() {
	if a == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = a.srv.Shutdown(ctx)
	_ = a.ln.Close() // Serve may not have taken the listener yet
}
