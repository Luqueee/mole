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

Four ways to install — pick whichever fits. All produce the same
single static binary: no runtime, no `node_modules`, no daemon.

### Option 1 — one-liner (no clone, no setup)

The installer script handles everything: detects the platform, builds
the binary, and copies it onto your `PATH`.

**Linux / macOS / FreeBSD:**

```bash
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh | sh
```

**Windows (PowerShell 5+):**

```powershell
iwr -useb https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.ps1 | iex
```

By default the script installs to:

| User           | Install location                  |
|----------------|-----------------------------------|
| root (Unix)    | `/usr/local/bin/mole`             |
| non-root (Unix)| `~/.local/bin/mole`               |
| Windows        | `%LOCALAPPDATA%\Programs\mole\`   |

If the destination isn't already on your `PATH`, the script prints
the exact line to add to your shell profile.

Useful flags:

```bash
# Linux/macOS — also launch the interactive configurator
# (only works in a real terminal; will fall back to non-interactive
# when piped, reading MOLE_* env vars — see Option 5 below)
./scripts/install.sh --init

# Linux/macOS — install under a custom prefix
./scripts/install.sh --prefix /opt

# Linux/macOS — pin a specific ref
MOLE_VERSION=v0.1.0 ./scripts/install.sh

# Windows — custom install dir
.\scripts\install.ps1 -InstallDir $env:LOCALAPPDATA\Programs\mole

# Windows — also run init
.\scripts\install.ps1 -Init
```

The one-liner flavour of `--init` is documented in
[Option 5 — fully automatic](#option-5--fully-automatic-scripted-zero-prompts).

### Option 2 — `go install` (no clone, requires Go 1.22+)

Drops the binary into `$(go env GOPATH)/bin` (usually `~/go/bin`):

```bash
go install github.com/Luqueee/mole/cmd/mole@latest
```

Make sure that directory is on your `PATH`:

```bash
# add to ~/.bashrc, ~/.zshrc, or your shell's equivalent
export PATH="$(go env GOPATH)/bin:$PATH"
```

### Option 3 — install script from a clone

If you already have the repo (or want the source locally), run the
same script directly — it skips the network clone:

**Linux / macOS / FreeBSD:**

```bash
git clone https://github.com/Luqueee/mole
cd mole
./scripts/install.sh
```

**Windows (PowerShell):**

```powershell
git clone https://github.com/Luqueee/mole
cd mole
./scripts/install.ps1
```

The installer picks the destination automatically (same table as
Option 1). Override with `--prefix` or the `INSTALL_DIR` env var:

```bash
./scripts/install.sh --prefix /opt            # → /opt/bin/mole
INSTALL_DIR=~/bin/mole ./scripts/install.sh   # → ~/bin/mole
```

### Option 4 — `make install` from a clone

Same as Option 3 but driven by `make`. Default destination is
`$(go env GOPATH)/bin/mole`:

```bash
git clone https://github.com/Luqueee/mole
cd mole
make install                      # → $(go env GOPATH)/bin/mole
make install PREFIX=/usr/local    # → /usr/local/bin/mole
make install INSTALL_DIR=~/bin/mole  # → ~/bin/mole
```

### Option 5 — fully automatic (scripted, zero prompts)

For CI, dotfiles, Dockerfiles, and `curl | sh` lovers: install the
binary **and** generate a working `mole.yaml` in a single
non-interactive pass. `mole init` reads its answers from flags or
environment variables when `-no-prompt` is set, so there is nothing
to type.

The installer scripts already auto-detect the non-TTY stdin and
forward `-no-prompt` to `mole init` for you — no extra flags
needed in the one-liner.

**Linux / macOS / FreeBSD:**

```bash
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh \
  | MOLE_REMOTE=dev@workstation \
    MOLE_PORTS=3000,5173,8080 \
    MOLE_AUTO_DISCOVER=true \
    sh -s -- --init
```

**Windows (PowerShell):**

```powershell
$env:MOLE_REMOTE        = 'dev@workstation'
$env:MOLE_PORTS         = '3000,5173,8080'
$env:MOLE_AUTO_DISCOVER = 'true'
iwr -useb https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.ps1 | iex -Init
```

What you get:

- the binary is built (or downloaded) and dropped onto `PATH`;
- the installer detects the non-TTY stdin and runs
  `mole init -no-prompt`, which reads the `MOLE_*` env vars and
  writes `./mole.yaml` (or `~/.config/mole/config.yaml` if
  `MOLE_GLOBAL=true`) without asking a single question;
- the install is reproducible — pin a ref with `MOLE_VERSION=v0.1.0`.

Minimal example (auto-discover, no explicit ports):

```bash
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh \
  | MOLE_REMOTE=dev@workstation MOLE_AUTO_DISCOVER=true \
    sh -s -- --init
```

Add `MOLE_GLOBAL=true` to install the config at
`~/.config/mole/config.yaml` (per-user) instead of `./mole.yaml`
(per-project):

```bash
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh \
  | MOLE_REMOTE=dev@workstation MOLE_AUTO_DISCOVER=true MOLE_GLOBAL=true \
    sh -s -- --init
```

Append `-test` to also probe the SSH connection right after writing
the config. The simplest pattern is to do the install without
`--init` and run `mole init -test` yourself so you can pass
explicit flags:

```bash
# 1. install the binary
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh | sh

# 2. write a config and smoke-test the SSH connection
mole init -no-prompt -test -force \
  -remote dev@workstation \
  -ports 3000,5173,8080 \
  -auto-discover
```

### Build without installing

If you just want the binary in the project tree (e.g. for CI):

```bash
make build      # → ./dist/mole
# or, with go directly:
go build -trimpath -o ./mole ./cmd/mole
```

### Uninstall

The uninstaller mirrors the installer and is one command either way:

```bash
# from a clone
./scripts/uninstall.sh                 # Unix
./scripts/uninstall.ps1                # Windows

# one-liner (no clone)
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/uninstall.sh | sh
iwr -useb https://raw.githubusercontent.com/Luqueee/mole/main/scripts/uninstall.ps1 | iex

# add --purge to also drop ~/.config/mole/ (Unix) or
# %LOCALAPPDATA%\mole\ (Windows)
./scripts/uninstall.sh --purge

# or, with make:
make uninstall
```

For `go install`, simply remove the binary from
`$(go env GOPATH)/bin/mole`.

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

### Generating the config with `mole init`

`mole init` is the canonical way to produce a `mole.yaml`. It is
shipped with the binary — there is **no separate `init` script** —
so the prompts are identical on every OS and the schema is always
in sync with the loader. The installer scripts call it for you
when you pass `--init` / `-Init`.

Three modes, same binary:

| Mode | When | How |
|------|------|-----|
| **Interactive** | first-time setup on a new machine | `mole init` |
| **Semi-interactive** | you know the remote, want a sensible default for the rest | `mole init -remote dev@workstation` |
| **Fully scripted** | CI, Dockerfiles, dotfiles, no TTY | `mole init -no-prompt -remote … [-ports …] [-auto-discover]` |

A typical interactive run:

```bash
$ mole init
configuring mole — press Enter to accept the default in [brackets]
SSH remote (user@host[:port]) [dev@workstation]:
How should mole pick ports?
  1) auto-discover common dev ports (recommended)
  2) explicit list (comma-separated)
  3) skip — I'll configure ports later
  choose [1]: 1
Where to save the config?
  1) ./mole.yaml               (current directory, project-local)
  2) ~/.config/mole/config.yaml  (user-global)
  3) don't save — print to stdout instead
  choose [1]: 1
wrote ./mole.yaml
```

**Scripted (no TTY) example** — generate a config and a global
install in one shot:

```bash
mole init -no-prompt \
  -remote dev@workstation \
  -ports 3000,5173,8080 \
  -auto-discover \
  -global \
  -test \
  -force
```

- `-no-prompt`  refuse to ask; fail if a required value is missing.
- `-global`     write to `~/.config/mole/config.yaml` (Windows:
  `%APPDATA%\mole\config.yaml`) instead of `./mole.yaml`.
- `-print`      print the rendered YAML to stdout, don't write a
  file. Useful for `mole init -print > mole.yaml`.
- `-test`       after writing, run `ssh -o BatchMode=yes -o
  StrictHostKeyChecking=accept-new <remote> true` as a connectivity
  smoke test.
- `-force`      overwrite an existing config file.
- `-yes`        accept defaults for any unanswered interactive
  prompt (combine with `-remote` to answer only the questions the
  flag didn't cover).

**Environment variables** (read when the corresponding flag is
empty; honored in every mode):

| Variable               | Maps to flag        | Example                       |
|------------------------|---------------------|-------------------------------|
| `MOLE_REMOTE`          | `-remote`           | `dev@workstation`             |
| `MOLE_PORTS`           | `-ports`            | `3000,5173,8080`              |
| `MOLE_AUTO_DISCOVER`   | `-auto-discover`    | `true` / `false`              |
| `MOLE_CONFIG_PATH`     | `-config`           | `/etc/mole.yaml`              |
| `MOLE_GLOBAL`          | `-global`           | `true` / `false`              |

**TTY behaviour:** `mole init` only asks questions when stdin is a
TTY. When invoked from a pipe (the typical `curl | sh` install) it
errors out unless `-no-prompt` is passed. The install scripts
already handle this for you — passing `--init` from a real terminal
runs the interactive wizard, while passing it from a pipe (or with
`iex` on Windows) automatically adds `-no-prompt` so the
`MOLE_*` env vars drive everything.

This is what makes the "fully automatic" one-liner work — see
[Option 5](#option-5--fully-automatic-scripted-zero-prompts) in
the install section above.

If you ever need to know exactly which path a `-global` install
would write to, run `mole init -print -global -no-prompt -remote
user@host` (it prints to stdout, no file is created).

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

```
mole init [flags]

  -remote <target>   SSH target, e.g. dev@workstation[:port]
  -ports <list>      comma-separated ports to forward (e.g. 3000,5173)
  -auto-discover     probe the remote for common dev ports
  -config <path>     where to write the YAML (default: ./mole.yaml)
  -global            write to the user-global config (~/.config/mole/...)
  -print             print the generated YAML to stdout instead of writing
  -no-prompt         don't ask; require all values via flags / env vars
  -yes               accept defaults for any unanswered questions
  -test              after writing, test the SSH connection
  -force             overwrite the config file if it already exists

Environment (read when the corresponding flag is empty):
  MOLE_REMOTE, MOLE_PORTS, MOLE_AUTO_DISCOVER,
  MOLE_CONFIG_PATH, MOLE_GLOBAL
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
make build      # → ./dist/mole
make install    # build + install to $(go env GOPATH)/bin/mole (or PREFIX=/BIN)
make uninstall  # remove the installed binary
make test       # go test ./...
make tidy
make clean
```

Or via the cross-platform scripts:

```bash
./scripts/install.sh
./scripts/uninstall.sh            # add --purge to also drop ~/.config/mole/
# Windows:
#   ./scripts/install.ps1
#   ./scripts/uninstall.ps1
```

## License

MIT.