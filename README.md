# mole

> Hit `localhost:3000` as if it were always running — even when the
> real service lives on another machine.

`mole` opens a single SSH connection to a remote host and forwards
one or more local TCP ports through it to the same port numbers on
the remote. Optional auto-discover mode probes the remote for common
dev ports and forwards the ones that respond.

```
┌────────────┐    SSH tunnel (single conn)    ┌─────────────────┐
│  Browser   │ ───► localhost:3000 ───►  ───► │ workstation:3000│
│  curl      │ ───► localhost:5173 ───►  ───► │ workstation:5173│
│  whatever  │ ───► localhost:8080 ───►  ───► │ workstation:8080│
└────────────┘                                 └─────────────────┘
              ▲
              │  mole runs here
```

## Features

- **Single SSH connection** for many forwarded ports.
- **Auto-discover**: probe the remote for common dev ports and only
  forward the ones that respond.
- **Auto-reconnect**: transparent reconnect on tunnel drop, with
  periodic health checks.
- **Multi-auth**: ssh-agent first (Unix socket on Linux/macOS, named
  pipe on Windows), then `~/.ssh/id_*` keys.
- **Admin API**: `GET /status` for live stats, `GET /health` for
  liveness probes.
- **Single binary**: no runtime, no `node_modules`.
- **Graceful shutdown** on `SIGINT` / `SIGTERM`.

## Install

```bash
go install github.com/Luqueee/mole/cmd/mole@latest
```

Or build from source:

```bash
git clone https://github.com/Luqueee/mole
cd mole
make build
# binary at ./dist/mole
```

## Usage

### One-liner (auto-discover)

```bash
mole up --remote dev@workstation --auto-discover
```

The proxy opens an SSH connection, probes a built-in list of common
dev ports (3000, 3001, 5173, 8080, 9000, …) on the remote, and
forwards the ones that respond.

### Explicit ports

```bash
mole up --remote dev@workstation --ports 3000,5173,8080
```

### Config file

Drop a `mole.yaml` in your project root (see
[`examples/mole.yaml`](examples/mole.yaml)):

```yaml
remote: dev@workstation
ports: [3000, 5173, 8080]
auto_discover: true
log_level: info
```

Then:

```bash
mole up
```

### Status

```bash
mole status
# {"stats":{"uptime":"1m23s","active_conns":0,"total_conns":42,...},"info":{...}}
```

```bash
curl http://127.0.0.1:9999/status
curl http://127.0.0.1:9999/health   # → ok
```

## CLI reference

```
mole up [flags]

  -config         path to YAML config (default "mole.yaml")
  -remote         SSH target, e.g. dev@workstation[:port]
  -ports          comma-separated ports to forward (e.g. 3000,5173)
  -auto-discover  probe remote for common dev ports
  -admin          admin HTTP address (empty to disable)
  -log-level      debug|info|warn|error

mole status [-admin 127.0.0.1:9999]
mole version
mole help
```

## Config reference

| Field            | Type     | Default                | Notes                                       |
|------------------|----------|------------------------|---------------------------------------------|
| `remote`         | string   | —                      | SSH target, `user@host[:port]` (required)   |
| `ports`          | int[]    | `[]`                   | Explicit ports to forward                   |
| `auto_discover`  | bool     | `false`                | Probe remote for common dev ports           |
| `discover_ports` | int[]    | see below              | Override the probe list                     |
| `admin_addr`     | string   | `127.0.0.1:9999`       | Admin HTTP address (empty to disable)        |
| `log_level`      | string   | `info`                 | `debug`, `info`, `warn`, `error`            |
| `ssh_port`       | int      | `22`                   | SSH port on the remote                      |

Default `discover_ports`:

```
3000 3001 3002 3003 3004 3005
4200 5173 5174 5327
6006 8000 8080 8081 8443 9000 9090
```

## How it works

1. Open **one** SSH client connection to the remote (using your
   ssh-agent or default `~/.ssh/id_*` keys).
2. For each port to forward, bind `127.0.0.1:<port>` locally.
3. When a client connects locally, dial `127.0.0.1:<port>` through the
   SSH tunnel and bridge bytes bidirectionally.
4. A background goroutine periodically probes the SSH session and
   reconnects if it has died.

## Platforms

Single static Go binary — runs on **Linux**, **macOS**, and **Windows**
(amd64 and arm64). SSH authentication is native on each platform:

- **Linux / macOS / BSD**: ssh-agent over a Unix domain socket
  (`SSH_AUTH_SOCK`).
- **Windows**: ssh-agent over the OpenSSH named pipe
  (`\\.\pipe\openssh-ssh-agent`, or whatever `SSH_AUTH_SOCK` points to).

## Limitations

- **Host key verification is off** (`InsecureIgnoreHostKey`). Fine for
  dev, not for untrusted networks. TODO: known_hosts support in v0.2.
- **No daemonization**: runs in foreground. Use your shell's job
  control or wrap it (systemd, tmux, `nohup`, …).
- **TCP only**: no UDP forwarding yet.
- **Pageant** (PuTTY's Windows agent) is not supported — only the
  OpenSSH agent on Windows.

## Development

```bash
make build   # → ./dist/mole
make test    # go test ./...
make clean
```

## License

MIT.