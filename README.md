<div align="center">

# 🦫 mole

**Hit `localhost:3000` as if it were always running — even when the real service lives on another machine.**

[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Platforms](https://img.shields.io/badge/platforms-Linux%20·%20macOS%20·%20Windows%20·%20FreeBSD-555)](#-platforms)
[![Single binary](https://img.shields.io/badge/deploy-single%20static%20binary-2ea043)](#-install)
[![License](https://img.shields.io/badge/license-MIT-blue)](#-license)

</div>

---

`mole` opens **one** SSH connection to a remote host and forwards local TCP
ports through it to the same ports on the remote. With auto-discovery, it
checks the remote's TCP listeners or probes a configured port list when
listener enumeration is unavailable. Run it in the background to pick up new
dev servers as they start.

```
┌────────────┐      one SSH connection        ┌────────────────────┐
│  browser   │ ──►  localhost:3000  ──►──────►│  workstation:3000  │
│  curl      │ ──►  localhost:5173  ──►──────►│  workstation:5173  │
│  whatever  │ ──►  localhost:8080  ──►──────►│  workstation:8080  │
└────────────┘             ▲                  └────────────────────┘
                           │  mole runs here, on your laptop
```

## ✨ Features

| | |
|---|---|
| 🔌 **One connection** | A single SSH client multiplexes every forwarded port. |
| 🔭 **Smart auto-discover** | Enumerates the remote's TCP listeners with `ss` or `netstat`; if neither works, probes `discover_ports`. Re-scans every 15s to pick up new servers. |
| 🧭 **SSH config aliases** | A `remote` like `dev` is resolved through `~/.ssh/config` (`ssh -G`): HostName, User, Port, IdentityFile, Include, Match — all honoured. |
| 🛡️ **Exclude list** | System/reserved ports (22, 25, 53, 111, 631) are skipped by default; fully configurable. |
| ♻️ **Auto-reconnect** | Transparent reconnect on tunnel drop, with periodic health checks. |
| 🔑 **Native auth** | ssh-agent first (Unix socket / Windows named pipe), then `~/.ssh/id_*` keys. |
| 📨 **Send files** | `mole send` puts files in the other machine's inbox, from either side, over the same SSH connection; a picker lists the folder when you name nothing. |
| 🕸️ **Manage other machines** | Register nodes with `mole node add`, then run any mole command on one with `mole -n NODE <command>`: its ports, pushes, inbox, status, update. Any machine can manage any other it can reach over SSH. |
| 🌙 **Background daemon** | `mole up -d` detaches; `mole down` stops it; `mole status` and `mole logs` introspect it. |
| 🎨 **Beautiful logs** | `mole logs` renders the daemon log with colour level badges, a green `FORWARD` badge, and `(×N)` collapsing of repeats. |
| 📦 **Single binary** | No separate runtime or `node_modules`; daemon mode uses the same executable. |

## 🎨 Beautiful logs

`mole logs` renders the daemon's structured log with coloured level badges, a
distinct green **FORWARD** badge when a port starts forwarding and a burnt-orange
**UNFWD** badge when a dead remote port is pruned, dimmed timestamps, and
`(×N)` collapsing of repeated lines:

<div align="center">
  <img src="docs/mole-logs.png" alt="mole logs — colourised daemon log with level badges, a green FORWARD badge, an UNFWD badge, and (×N) collapsing" width="920">
</div>

## 🚀 Quickstart

Install on **Windows** with PowerShell 5.1 or newer:

```powershell
irm https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.ps1 | iex
```

On **Linux, macOS, or FreeBSD**, install with Go 1.26.4+ and Git available:

```bash
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh | sh
```

Open a new terminal if needed, then start mole with an SSH `Host` alias from
your `~/.ssh/config`. On a fresh install, choose how to select ports:

```text
mole up --remote devlabs --auto-discover -d
mole logs -f
mole down
```

Replace `devlabs` with your own SSH alias. Press Ctrl+C to stop following logs;
the daemon keeps running until `mole down`.

Use `--ports 3000,5173` instead of `--auto-discover` to forward specific ports.
If `./mole.yaml` or your user-global config already sets `auto_discover: true`
or `ports:`, use the shorter command:

```text
mole up --remote devlabs -d
```

The `-d` flag starts mole in the background. Run `mole init` if you want to
create a config interactively.

## 🧰 Commands

| Command | What it does |
|---------|--------------|
| `mole up` | Start the forwarder in the foreground (add `-d` to background it). |
| `mole down` | Stop a backgrounded mole (started with `up -d`). |
| `mole restart` | Stop and re-launch a backgrounded mole using the same config. |
| `mole status` | Query the local admin API for live stats + forwarded ports. |
| `mole logs` | Show the daemon log, colourised; `-f` to follow, `clean` to truncate it. |
| `mole ports` | Manage the auto-discover port list: `add`, `remove`/`rm`, `list`/`ls`. |
| `mole push` · `mole unpush` | Expose a local port on the remote (`ssh -R`): `push LOCAL[:REMOTE]`, `push list`, `unpush LOCAL`. |
| `mole send` (`ftp`) | Send files to the other machine's inbox; no arguments opens a file picker: `send [-to SUBFOLDER] [files...]`. |
| `mole inbox` | Where received files land, and who may send: `inbox`, `path`, `set DIR`, `receive on\|off`, `mode ask\|auto`, `pending`, `accept`, `reject`. |
| `mole node` | The other machines you manage: `node add NAME TARGET`, `node ls`, `node ping NAME`, `node rm NAME`. |
| `mole -n NODE <command>` | Run any mole command on a node, as if typed there (pickers and Ctrl+C work). |
| `mole doctor` | Diagnose config, SSH, push and split DNS with the fix for each problem: `doctor [name...]`. |
| `mole dns` | Route configured `domains:` to the remote's DNS: `dns install`, `dns uninstall` (Linux, root). |
| `mole config` | `config edit` opens the active `mole.yaml` in `$VISUAL` / `$EDITOR`. |
| `mole init` | Generate a `mole.yaml` interactively (or scripted). |
| `mole clip` | Share clipboard images over a separate private HTTP connection: `clip serve`, `clip pull`. |
| `mole update` | Update in place using the installer (latest source on Unix, latest release on Windows). |
| `mole version` · `mole help` | The obvious. |

## 📦 Install

### Installers

Outside a local mole clone, the Windows installer downloads the latest release
for amd64 or arm64, checks its SHA-256, and adds the install directory to your
user `PATH`. It needs PowerShell 5.1 or newer, but no Go or Git:

```powershell
irm https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.ps1 | iex
```

The Unix installer builds from a local mole clone when run inside one;
otherwise it clones the latest `main`. This one-liner needs Go 1.26.4+, Git,
and curl:

```bash
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh | sh
```

| Platform | Default binary location |
|----------|-------------------------|
| Windows | `%LOCALAPPDATA%\Programs\mole\mole.exe` |
| Linux, macOS, FreeBSD (user) | `~/.local/bin/mole` |
| Linux, macOS, FreeBSD (root) | `/usr/local/bin/mole` |

Open a new PowerShell session to use the updated user `PATH`. On Unix, the
installer prints a `PATH` hint when needed.

To install from a local clone, run `./scripts/install.sh` on Unix or
`.\scripts\install.ps1` in PowerShell; both build from source with Go. You can
also download a [prebuilt archive](https://github.com/Luqueee/mole/releases/latest)
for any supported platform and check it against the release's `SHA256SUMS`.

Custom destinations and version pins:

```bash
./scripts/install.sh --prefix /opt              # /opt/bin/mole
INSTALL_DIR=~/bin/mole ./scripts/install.sh      # exact binary path
MOLE_VERSION=v0.1.1 ./scripts/install.sh         # Git ref, outside a clone
```

```powershell
.\scripts\install.ps1 -InstallDir 'C:\Tools\mole' # from a clone; requires Go
```

Outside a clone, pin a published Windows release before using the one-liner:

```powershell
$env:MOLE_VERSION = 'v0.1.1'
irm https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.ps1 | iex
```

The `-InstallDir` PowerShell option accepts a directory. The `INSTALL_DIR`
environment variable accepts the full path to `mole.exe`.

### Other ways to install

With Go 1.26.4+:

```bash
go install github.com/Luqueee/mole/cmd/mole@latest
```

Add your Go `GOPATH/bin` directory to `PATH` if needed. From a clone on Unix,
`make install` builds into `$(go env GOPATH)/bin`; `PREFIX=/usr/local` and
`INSTALL_DIR=/path/to/mole` override that destination.

### Scripted setup

After installing on any platform, create a user-global config without prompts:

```text
mole init -no-prompt -remote dev -auto-discover -global
```

Use `-ports 3000,5173` instead of `-auto-discover` for an explicit list.
`mole init` also reads `MOLE_REMOTE`, `MOLE_PORTS`, `MOLE_AUTO_DISCOVER`,
`MOLE_CONFIG_PATH`, and `MOLE_GLOBAL` when their matching flags are absent.

### Build without installing

```bash
make build      # → ./dist/mole
# or: go build -trimpath -o ./mole ./cmd/mole
```

### Update

Update an installed mole in place — no manual re-clone. It re-runs the official
installer against the running binary's own location:

```bash
mole update                  # latest main on Unix; latest release on Windows
mole update -version v0.1.1  # Unix: Git ref; Windows: release tag
mole update -branch feat/x   # build and install a branch (see below)
mole update -dry-run         # print what it would run, change nothing
```

Unix updates need Go, Git, and curl or wget. Windows release updates need
PowerShell. For a `go install` setup, re-run
`go install github.com/Luqueee/mole/cmd/mole@latest` instead.

#### Try a branch

You do not need a release to test something: `-branch` builds a branch from
source and installs it over the running binary.

```bash
mole update -branch feat/mesh-nodes      # this machine
mole -n devlabs update -branch feat/x    # another node
mole version                             # mole v0.3.0 (feat/mesh-nodes@a1b2c3d)
mole update                              # back to main
```

`mole version` (and `mole node ping`) show which branch and commit a binary
was built from, so you can tell what each machine runs. The installer fetched
is the one **that branch** carries, so a branch can change the installer too.
A branch that does not exist is an error, never a silent install of `main`.
On Windows `-branch` downloads the branch's source and builds it, so it needs
Go (releases do not). Without the CLI:

```bash
MOLE_VERSION=feat/x curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/refs/heads/feat/x/scripts/install.sh | sh
```

`MOLE_REPO` points the installer at a fork (or a local repository), and
`MOLE_RAW_URL` at another place to fetch the installers from.

### Uninstall

```bash
./scripts/uninstall.sh         # Unix
./scripts/uninstall.sh --purge # also remove the user config
```

On Windows, run `.\scripts\uninstall.ps1` from a clone. For a `go install`
setup, remove the binary from `$(go env GOPATH)/bin`.

## 🛠️ Usage

### Foreground vs background

```bash
mole up                 # foreground (Ctrl+C to stop) — great for debugging
mole up -d              # background daemon — returns your shell
mole down               # stop the daemon
```

### Pick the remote

The `remote` can be an explicit target **or an SSH config Host alias**:

```bash
mole up --remote dev@workstation     # explicit user@host[:port]
mole up --remote dev                 # alias from ~/.ssh/config (resolved via ssh -G)
```

### Pick the ports

```bash
mole up --remote dev --auto-discover           # forward whatever's listening on the remote
mole up --remote dev --ports 3000,5173,8080    # forward an explicit set
```

With `--auto-discover`, mole tries `ss` or `netstat` on the remote and forwards
listeners reachable through remote loopback, excluding `exclude_ports`. If
neither command works, it probes only the ports in `discover_ports`. It repeats
discovery every 15 seconds, so a new server can be forwarded without a restart.

### Config file

Drop a `mole.yaml` in your project (or generate it with `mole init`):

```yaml
remote: dev                    # user@host[:port] or an ssh config alias
auto_discover: true
exclude_ports: [22, 25, 53, 111, 631]   # never auto-forwarded; [] excludes nothing
admin_addr: 127.0.0.1:9999
log_level: info
```

`mole up` looks for `./mole.yaml` first, then the user-global config:
`~/.config/mole/config.yaml` on Unix or `%APPDATA%\mole\config.yaml` on Windows.
`mole init -global` writes to that user-global location.

### Inspect a running daemon

```bash
mole status        # JSON: uptime, connections, and the live forwarded-port set
mole logs          # pretty, colourised log (last 200 lines)
mole logs -f       # follow, like tail -f
mole logs -n 50 --no-dedup
```

`mole status` needs the admin API to be on. It's **off by default** — set
`admin_addr: 127.0.0.1:9999` in the config (`mole init` writes it for you) or
pass `mole up -admin 127.0.0.1:9999`.

`mole logs` parses the daemon's structured log and renders it with coloured
level badges (a distinct green **FORWARD** badge for forwarded ports), a dimmed
timestamp, and collapses consecutive identical lines into one with a `(×N)`
counter. Colour auto-disables when piped or when `NO_COLOR` is set (`--color`
forces it back on).

### Restart, ports, and editing the config

```bash
mole restart                 # stop and re-launch the daemon with the same config

mole ports list              # show the auto-discover port list
mole ports add 4321          # add a discovery candidate
mole ports remove 4321       # or: mole ports rm 4321

mole config edit             # open the active mole.yaml in $VISUAL / $EDITOR
mole config edit -editor nvim

mole logs clean              # truncate the daemon log
mole logs clean -keep 200    # …keeping the last N lines
```

`mole ports` saves changes to the active config. With the admin API enabled,
it also attempts to apply them to a running daemon; otherwise restart mole to
pick up the updated list.

### Push: expose a local service on the remote

`mole up` normally *pulls* remote ports to `localhost` (`ssh -L`). `push` is the
opposite (`ssh -R`), over the same SSH connection: a service on **your machine**
becomes reachable from the **remote**.

```bash
mole push 3000:8080          # on the remote, localhost:8080 now reaches your :3000
mole push 3000              # same port on both sides
mole push list              # forwards and their state (active | failed: reason)
mole unpush 3000            # close the remote listener
```

```yaml
push:
  - local: 3000
    remote: 8080     # default: same as local
    bind: 127.0.0.1  # address on the remote (default: loopback)
```

Like `mole ports`, changes are saved to the config and applied live if the
daemon is running. Push forwards are always explicit — never auto-discovered.
They are re-opened automatically after an SSH reconnect.

### Send files between the two machines

Each machine has an **inbox**, the folder where files sent to it land
(`~/mole-inbox` by default). `mole send` puts files in the *other* machine's
inbox; you never type a destination path, only (optionally) a sub-folder of it.

```bash
mole send report.pdf              # to the remote's inbox
mole send a.txt b.txt -to docs    # into <remote inbox>/docs
mole send                         # no files: pick them from this folder
mole ftp build.tar.gz             # ftp is an alias (it is not the FTP protocol)
```

With no files, in a terminal, a multi-select lists the regular files of the
current folder (`space` selects, `ctrl+a` all, `/` filters, `enter` sends; `-a`
includes hidden files). Directories are not supported yet.

**Local to remote** needs nothing to be enabled: run it where `mole up` runs. The
daemon opens an SSH session on the remote that runs `mole recv --stdio`, so SSH
itself is the authentication and nothing is exposed. `mole` must be installed on
the remote, and recent enough to know `recv`.

**Remote to local** is opt-in, because it lets a process on the remote write
into this machine:

```bash
mole inbox receive on        # on the machine that runs mole up
mole send report.pdf         # on the remote: lands in the local inbox
```

The daemon listens on a loopback port and exposes it on the remote through a
hidden push forward (`transfer.port`, default 9977; it does not appear in
`mole push list`). A random token, readable only by your user on both machines,
must accompany every transfer, so other users on the remote cannot use the port.

Incoming transfers **wait for your answer** unless you set `mole inbox mode auto`:

```bash
mole inbox pending           # who is sending what
mole inbox accept            # or: accept 3, accept all, reject ...
```

The desktop app has a **Transfers** tab for all of this, with drag and drop, a
file chooser, progress, history, and accept/reject buttons; the tray notes
transfers that are waiting.

```yaml
transfer:
  inbox: ~/mole-inbox   # where files sent to THIS machine land (absolute or ~/)
  receive: false        # accept files sent from the remote
  accept: ask           # ask | auto
  port: 9977            # loopback port for remote-to-local transfers
  max_size_mb: 0        # cap on one incoming transfer (0 = none)
```

Files are checked with SHA-256, written under a temporary name and only then
renamed, and never overwrite: a name that exists becomes `name (1).ext`. Names
that try to leave the inbox (`../x`, absolute paths, symlinked folders) are
refused, and received files keep only the owner's permissions. Set the remote's
own inbox with `mole inbox set DIR` on the remote.

### Manage other machines (nodes)

Every machine can manage the others it can reach over SSH: there is no fixed
"orchestrator". Register a node once, then run any mole command on it from
here:

```bash
mole node add devlabs devlabs          # NAME, then user@host[:port] or an ssh alias
mole node ls                           # who answers, which mole they run, daemon state
mole -n devlabs ports add 3000         # edit devlabs' own config and apply it live
mole -n devlabs push 8080
mole -n devlabs inbox mode auto
mole -n devlabs status
mole -n devlabs send                   # the file picker opens on devlabs, in your terminal
mole -n devlabs update                 # update mole on devlabs
```

The command runs there exactly as if you had typed it on that machine: its
config, its daemon, its inbox. It needs a `mole` on the node (a node without
`agent` support, i.e. older than 0.4, still works with `-n`, it is just listed
as "no agent"). Arguments are quoted, so they reach the node verbatim, and the
node's exit code is yours. Authentication is SSH's: whoever can open a shell
there can configure it, nobody else.

```yaml
nodes:
  devlabs:
    remote: devlabs            # user@host[:port] or an ssh alias
    ssh_port: 22               # optional
    bin: ~/.local/bin/mole     # optional: where mole is on the node (default: mole)
    insecure: false            # optional, UNSAFE: skip host key verification
```

Nothing changes for existing setups: with no `nodes:`, mole behaves as before,
and the config's own `remote:` also works as a node name (`mole -n <remote> ...`).
Nodes are kept in the user-global config (`mole node add` writes it when no
config exists), so any project sees them.

### Resolve a domain on the remote (split DNS)

To reach names that only the remote can resolve (e.g. `admin.kena.lan`) from
your browser with no proxy settings:

```yaml
domains: [kena.lan]
domain_ports: [80, 443]   # optional (default)
```

```bash
sudo mole dns install     # once per machine: mole0 link + nftables + boot unit
mole up
curl https://admin.kena.lan
sudo mole dns uninstall
```

mole runs a tiny DNS stub for `*.kena.lan` (only those names — your normal DNS
is untouched), answers with a virtual `127.77.x.y` IP, and forwards connections
to it through SSH **by name**, so the remote's `sshd` does the real DNS
resolution. nftables redirects the domain ports to unprivileged listeners, so
mole coexists with local servers already owning `:80`/`:443`. Requires Linux
with `systemd-resolved` and `nft`.

### Diagnose with `mole doctor`

When something doesn't work, `mole doctor` checks the config, the SSH login,
whether the remote `sshd` allows pull *and* push forwarding, the daemon, push
forwards and the split-DNS setup, and prints a verdict per line with the fix
under each problem. Pass names to probe them end to end (remote DNS → local
resolution → TCP connect):

```text
$ mole doctor admin.kena.lan
  ❯ SSH
    ✓  connect              authenticated in 628ms
    ✓  pull forwarding      direct-tcpip channels open
    ✗  push forwarding      sshd refused the remote listener
       ↳ set `AllowTcpForwarding yes` in the remote sshd_config
```

The exit status is 1 if any check failed. It never changes anything.

### Clipboard images over a private network

`mole clip` moves clipboard **images** from the machine running `clip serve` to
the one running `clip pull` — useful for pasting a screenshot taken on a remote
desktop. It uses a separate HTTP connection over a private network, such as
Tailscale or WireGuard. Automatic clipboard watching is available on macOS;
`clip pull` works on every supported platform.

```bash
mole clip serve              # on the source Mac; watches its clipboard by default
mole clip pull               # on the target; uses clip_url from the config, or -url
```

The clip endpoint has no authentication and binds to `127.0.0.1:7777` by
default. For a remote pull, bind it to the source machine's private network
address and use the same address in `clip_url`:

```yaml
clip_url: http://100.64.0.10:7777
clip_listen: 100.64.0.10:7777
```

Binding to `0.0.0.0:7777`, `:7777`, or `[::]:7777` is supported for controlled
networks, but `mole clip serve` emits a warning because every interface can
reach the unauthenticated endpoint.


### Generate the config with `mole init`

`mole init` ships **inside the binary** — no separate script — so the prompts
are identical on every OS and always in sync with the loader.

| Mode | When | How |
|------|------|-----|
| **Interactive** | first-time setup | `mole init` |
| **Semi-interactive** | you know the remote | `mole init -remote dev` |
| **Fully scripted** | CI, Docker, no TTY | `mole init -no-prompt -remote … [-auto-discover]` |

```text
$ mole init
configuring mole — press Enter to accept the default in [brackets]
SSH remote (user@host[:port] or ssh config alias): dev
How should mole pick ports?
  1) auto-discover common dev ports (recommended)
  2) explicit list (comma-separated)
  3) skip — I'll configure ports later
  choose [1]: 1
Where to save the config?
  1) ./mole.yaml               (current directory, project-local)
  2) ~/.config/mole/config.yaml  (user-global)
  3) don't save — print to stdout instead
  choose [1]: 2
wrote ~/.config/mole/config.yaml
Start mole now? [Y/n]: y
starting mole in the background
```

Useful `init` flags: `-global`, `-print`, `-no-prompt`, `-yes`, `-test`,
`-force`, `-up`. Environment fallbacks (read when the matching flag is empty):
`MOLE_REMOTE`, `MOLE_PORTS`, `MOLE_AUTO_DISCOVER`, `MOLE_CONFIG_PATH`,
`MOLE_GLOBAL`.

## 📖 CLI reference

```text
mole up [flags]
  -config         path to YAML config (default: ./mole.yaml, then user-global)
  -remote         SSH target (user@host[:port]) or an ssh config alias
  -ports          comma-separated ports to forward (e.g. 3000,5173)
  -auto-discover  forward whatever is listening on the remote
  -admin          admin HTTP address (empty to disable)
  -log-level      debug|info|warn|error
  -insecure       disable SSH host key verification (UNSAFE; dev only)
  -d, -detach     run in the background; stop with 'mole down'

mole down
mole restart [-config PATH]
mole status  [-admin 127.0.0.1:9999]
mole logs    [-f] [-n N] [-raw] [-color] [-no-color] [-no-dedup]
mole logs clean [-keep N]
mole ports  add [-config PATH] <port>
mole ports  remove|rm [-config PATH] <port>
mole ports  list|ls [-config PATH]
mole push   [-config PATH] [-bind ADDR] LOCAL[:REMOTE]
mole push   list|ls [-config PATH]
mole unpush [-config PATH] LOCAL
mole send   [-config PATH] [-to SUBFOLDER] [-a] [-direct] [files...]    (alias: ftp)
mole inbox  [path | set DIR | receive on|off | mode ask|auto | pending | accept [ID|all] | reject [ID|all]] [-config PATH]
mole -n NODE <command> [args...]                 run a mole command on a node
mole node   add NAME TARGET [-ssh-port N] [-bin PATH] [-insecure] [-no-check] [-config PATH]
mole node   ls|ping NAME|rm NAME [-config PATH]
mole dns    install|uninstall [-config PATH]      (Linux, needs root)
mole doctor [-config PATH] [-timeout 10s] [name...]
mole config edit [-config PATH] [-editor CMD]
mole init   [flags]
mole clip   serve [-listen 127.0.0.1:7777] [-watch] [-config PATH] [-log-level L]
mole clip   pull  [-url URL] [-config PATH] [-log-level L]
mole update [-version REF] [-dry-run] [-no-verify]
mole version · mole help
```

## ⚙️ Config reference

| Field            | Type   | Default              | Notes                                              |
|------------------|--------|----------------------|----------------------------------------------------|
| `remote`         | string | —                    | `user@host[:port]` **or** an ssh config alias (required) |
| `ports`          | int[]  | `[]`                 | Explicit ports — always forwarded                  |
| `auto_discover`  | bool   | `false`              | Forward the remote's live listeners                |
| `discover_ports` | int[]  | see below            | Fallback probe list when `ss`/`netstat` are absent |
| `exclude_ports`  | int[]  | `[22,25,53,111,631]` | Never auto-forwarded; `[]` excludes nothing        |
| `admin_addr`     | string | `""` (disabled)      | Admin HTTP address; `mole init` writes `127.0.0.1:9999`. Required by `mole status` |
| `log_level`      | string | `info`               | `debug`, `info`, `warn`, `error`                   |
| `ssh_port`       | int    | `22`                 | SSH port on the remote                             |
| `insecure`       | bool   | `false`              | Disable SSH host key verification (UNSAFE; dev only) |
| `clip_url`       | string | —                    | Clip server URL used by `mole clip pull`           |
| `clip_listen`    | string | `127.0.0.1:7777`     | Bind address; use a private Tailscale/WireGuard IP for remote access |
| `clip_interval_ms` | int  | —                    | Clipboard poll interval for `clip serve -watch`    |
| `push`           | rule[] | `[]`                 | Local services exposed on the remote (`local`, optional `remote`, optional `bind`; default bind `127.0.0.1`) |
| `transfer`       | object | see [Send files](#send-files-between-the-two-machines) | `inbox`, `receive`, `accept`, `port`, `max_size_mb` |
| `nodes`          | map    | `{}`                 | Other machines to manage (`remote`, optional `ssh_port`, `bin`, `insecure`); see [Manage other machines](#manage-other-machines-nodes) |
| `domains`        | string[] | `[]`               | DNS suffixes (e.g. `kena.lan`) resolved **on the remote**; needs `sudo mole dns install` |
| `domain_ports`   | int[]  | `[80,443]`           | Ports forwarded for every name under `domains` (1–45535) |

Fallback `discover_ports`:

```
3000 3001 3002 3003 3004 3005
4200 5173 5174 5327
6006 8000 8080 8081 8443 9000 9090
```

## 🔬 How it works

1. Open **one** SSH client connection to the remote (ssh-agent or `~/.ssh/id_*`
   keys; aliases resolved via `ssh -G`).
2. Discover ports: enumerate the remote's TCP listeners (`ss`/`netstat`), or
   fall back to probing `discover_ports`. Skip `exclude_ports`.
3. For each port, bind `127.0.0.1:<port>` locally; on a local connection, dial
   `127.0.0.1:<port>` through the tunnel and bridge bytes both ways.
4. Re-discover every 15s and forward anything new; a watchdog goroutine
   reconnects the SSH session if it dies.

State (pidfile + background log) lives in `~/.local/state/mole/` on Unix
(honouring `XDG_STATE_HOME`) and `%LOCALAPPDATA%\mole\` on Windows.

## 💻 Platforms

Single static Go binary — **Linux**, **macOS**, **Windows**, **FreeBSD** (amd64 & arm64).
SSH auth is native per platform:

- **Linux / macOS / BSD** — ssh-agent over a Unix socket (`SSH_AUTH_SOCK`).
- **Windows** — ssh-agent over the OpenSSH named pipe
  (`\\.\pipe\openssh-ssh-agent`).

## ⚠️ Limitations

- **Host keys are verified** against `~/.ssh/known_hosts`. Unknown hosts are
  trusted on first use (recorded automatically, like OpenSSH); a later key
  *mismatch* is refused as a possible MITM. Pass `--insecure` (or
  `insecure: true`) to turn verification off for throwaway dev hosts.
- **Dead ports are pruned only under auto-discover** — when `ss`/`netstat` can be
  enumerated, a remote service that stops has its local listener closed (you'll
  see an `UNFWD` line). Explicitly configured `ports:` are pinned and never
  pruned, and when enumeration isn't available mole only adds, never removes.
- **TCP only** — no UDP forwarding yet.
- **Pageant** (PuTTY's Windows agent) isn't supported — OpenSSH agent only.
- **Push forwards need `AllowTcpForwarding`** on the remote `sshd` (on by
  default). They bind `127.0.0.1` on the remote unless `bind:` says otherwise;
  exposing them beyond that needs `GatewayPorts` on the remote — mole never
  enables it. A bind failure is reported once and shown as `failed` in
  `mole push list` / `mole status`; it is retried after each reconnect.
- **`domains:` is Linux-only** (systemd-resolved + nftables, one `sudo mole dns
  install` per machine). Browsers with their own DNS-over-HTTPS bypass it.
  Every configured domain port must be free of conflicts on the virtual IPs;
  a name whose port can't be bound answers `SERVFAIL` rather than being
  published half-working.

## 🧑‍💻 Development

```bash
make build      # → ./dist/mole
make install    # build + install (PREFIX=/usr/local or INSTALL_DIR=… to override)
make run        # build, then run `mole up`
make test       # go test ./...
make tidy
make uninstall
make clean
```

## 📄 License

MIT.
