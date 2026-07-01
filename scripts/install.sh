#!/usr/bin/env bash
# scripts/install.sh — install mole (Unix: Linux, macOS, FreeBSD).
#
# What this script does:
#   1. Locates the source: uses the local repo if it is one, otherwise
#      clones https://github.com/Luqueee/mole into a temp dir.
#   2. Builds the binary with `go build -trimpath`.
#   3. Copies it to the right place (root → /usr/local/bin, otherwise
#      $HOME/.local/bin). Override with --prefix or INSTALL_DIR.
#   4. Verifies with `mole version`.
#   5. Prints a hint to add the install dir to PATH if it isn't yet,
#      and to run `mole init` to configure (mole init does NOT run
#      automatically — it's interactive, so you opt in).
#
# Configuration lives in a single place: the `mole init` subcommand
# inside the binary itself, shared with the Windows installer.
#
# Usage:
#   ./scripts/install.sh                       # from a clone
#   curl -fsSL .../install.sh | sh             # one-liner
#   ./scripts/install.sh --prefix /opt         # custom prefix
#   INSTALL_DIR=~/bin/mole ./scripts/install.sh
#
# Options:
#   --prefix <dir>      install under <dir>/bin/mole
#   --no-verify         skip the post-install version check
#   --init              also run `mole init` after install
#   -h, --help          show help

set -euo pipefail

# Resolve a filesystem path to this script, when there is one. Used
# only by resolve_source() to detect "am I running from inside a clone"
# (script_dir/.. is the repo). In a piped invocation
# (`curl ... | sh`) there is no file behind the script and SELF stays
# empty — that's fine, resolve_source() then falls through to cloning.
#
# IMPORTANT: do NOT read stdin here. When the script is piped to `sh`,
# the interpreter reads the script body *from stdin* as it executes.
# Consuming stdin (e.g. `cat > tmp`) would swallow the rest of the
# not-yet-parsed script, the shell would hit EOF, and nothing past
# that point would ever run — the installer would print nothing at
# all. usage() therefore prints embedded text instead of sed-reading
# this file, so it needs no real path.
#
#   1. BASH_SOURCE[0]   — bash, file invocation
#   2. $0               — POSIX sh, file invocation
SELF=""
# BASH_SOURCE is bash-only: the array-subscript syntax errors out
# under POSIX sh (dash, bash-in-POSIX-mode). Guard the expansion so
# the parser never sees it on a non-bash interpreter; the [ -f "$0" ]
# branch below handles the same case for POSIX sh.
if [ -n "${BASH_VERSION:-}" ]; then
	if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
		SELF="${BASH_SOURCE[0]}"
	elif [ -n "${0:-}" ] && [ -f "${0}" ]; then
		SELF="${0}"
	fi
else
	if [ -n "${0:-}" ] && [ -f "${0}" ]; then
		SELF="${0}"
	fi
fi

REPO="https://github.com/Luqueee/mole.git"
BINARY="mole"
DEFAULT_REF="main"
VERSION_REF="${MOLE_VERSION:-$DEFAULT_REF}"

# Colour setup — only when stderr is a TTY and NO_COLOR is unset.
if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
	C_STEP=$(printf '\033[1;36m')
	C_OK=$(printf '\033[32m')
	C_WARN=$(printf '\033[33m')
	C_ERR=$(printf '\033[31m')
	C_DIM=$(printf '\033[2m')
	C_BOLD=$(printf '\033[1m')
	C_MAGENTA=$(printf '\033[35m')
	C_GREEN=$(printf '\033[32m')
	C_RESET=$(printf '\033[0m')
else
	C_STEP="" C_OK="" C_WARN="" C_ERR="" C_DIM="" C_BOLD="" C_MAGENTA="" C_GREEN="" C_RESET=""
fi

step() { printf '%s==%s %s\n' "$C_STEP" "$C_RESET" "$*" >&2; }
ok()   { printf '  %s✓%s %s\n' "$C_OK" "$C_RESET" "$*" >&2; }
warn() { printf '  %s!%s %s\n' "$C_WARN" "$C_RESET" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$C_ERR" "$C_RESET" "$*" >&2; exit 1; }

usage() {
	# Embedded so it works identically whether the script is run from a
	# file or piped to `sh` (where no real path exists to sed-read).
	cat <<'EOF'
install mole (Unix: Linux, macOS, FreeBSD).

What this script does:
  1. Locates the source: uses the local repo if it is one, otherwise
     clones https://github.com/Luqueee/mole into a temp dir.
  2. Builds the binary with `go build -trimpath`.
  3. Copies it to the right place (root -> /usr/local/bin, otherwise
     $HOME/.local/bin). Override with --prefix or INSTALL_DIR.
  4. Verifies with `mole version`.
  5. Prints a hint to add the install dir to PATH if it isn't yet,
     and to run `mole init` to configure.

Usage:
  ./scripts/install.sh                       # from a clone
  curl -fsSL .../install.sh | sh             # one-liner
  ./scripts/install.sh --prefix /opt         # custom prefix
  INSTALL_DIR=~/bin/mole ./scripts/install.sh

Options:
  --prefix <dir>      install under <dir>/bin/mole
  --no-verify         skip the post-install version check
  --init              also run `mole init` after install
  -h, --help          show help
EOF
}

PREFIX=""
VERIFY="yes"
RUN_INIT="no"
while [ $# -gt 0 ]; do
	case "$1" in
	--prefix)
		[ $# -ge 2 ] || die "--prefix requires a value"
		PREFIX="$2"
		shift 2
		;;
	--prefix=*)
		PREFIX="${1#--prefix=}"
		shift
		;;
	--no-verify)
		VERIFY="no"
		shift
		;;
	--init)
		RUN_INIT="yes"
		shift
		;;
	-h|--help)
		usage
		exit 0
		;;
	*)
		die "unknown argument: $1 (try --help)"
		;;
	esac
done

# ---------------------------------------------------------------------------
# Step 1: locate or fetch the source
# ---------------------------------------------------------------------------

PROJECT_ROOT=""
CLEANUP_DIR=""

cleanup() {
	[ -n "$CLEANUP_DIR" ] && [ -d "$CLEANUP_DIR" ] && rm -rf "$CLEANUP_DIR"
}
trap cleanup EXIT

is_mole_repo() {
	local d="$1"
	[ -f "$d/go.mod" ] || return 1
	grep -q "github.com/Luqueee/mole" "$d/go.mod" 2>/dev/null || return 1
	[ -d "$d/cmd/mole" ] || return 1
}

resolve_source() {
	if [ -n "${MOLE_SRC:-}" ] && is_mole_repo "$MOLE_SRC"; then
		echo "$MOLE_SRC"; return 0
	fi
	if is_mole_repo "$PWD"; then
		echo "$PWD"; return 0
	fi
	local script_dir=""
	# $SELF was resolved at the top of the script and points at a
	# real file path in every supported invocation (file or piped),
	# so we no longer need a bash-only BASH_SOURCE guard here.
	if [ -n "$SELF" ] && [ -f "$SELF" ]; then
		script_dir="$(cd "$(dirname "$SELF")" && pwd 2>/dev/null || true)"
	fi
	if [ -n "$script_dir" ] && is_mole_repo "$script_dir/.."; then
		echo "$(cd "$script_dir/.." && pwd)"; return 0
	fi
	if ! command -v git >/dev/null 2>&1; then
		die "no mole repo in CWD and 'git' not available to clone one"
	fi
	local tmp
	tmp="$(mktemp -d)"
	step "cloning $REPO (ref: $VERSION_REF) into $tmp/mole"
	if ! git clone --depth 1 --branch "$VERSION_REF" "$REPO" "$tmp/mole" >/dev/null 2>&1; then
		warn "branch '$VERSION_REF' not found, falling back to default branch"
		git clone --depth 1 "$REPO" "$tmp/mole" >/dev/null
	fi
	CLEANUP_DIR="$tmp"
	echo "$tmp/mole"
}

PROJECT_ROOT="$(resolve_source)"
step "using source: $PROJECT_ROOT"

# ---------------------------------------------------------------------------
# Step 2: build
# ---------------------------------------------------------------------------

GO_BIN="${GO:-$(command -v go || true)}"
[ -n "$GO_BIN" ] || die "'go' is not installed. Install Go 1.22+ from https://go.dev/dl/ and re-run."

BUILD_DIR="$PROJECT_ROOT/dist"
mkdir -p "$BUILD_DIR"
step "building $BINARY"
(
	cd "$PROJECT_ROOT"
	"$GO_BIN" build -trimpath -o "$BUILD_DIR/$BINARY" ./cmd/mole
)
[ -f "$BUILD_DIR/$BINARY" ] || die "build did not produce $BUILD_DIR/$BINARY"
ok "built $BUILD_DIR/$BINARY"

# ---------------------------------------------------------------------------
# Step 3: install
# ---------------------------------------------------------------------------

resolve_dest() {
	if [ -n "${INSTALL_DIR:-}" ]; then
		echo "$INSTALL_DIR"; return
	fi
	if [ -n "$PREFIX" ]; then
		echo "$PREFIX/bin/$BINARY"; return
	fi
	if [ "$(id -u)" = "0" ]; then
		echo "/usr/local/bin/$BINARY"
	else
		echo "$HOME/.local/bin/$BINARY"
	fi
}

DEST="$(resolve_dest)"
DEST_DIR="$(dirname "$DEST")"
step "installing to $DEST"
mkdir -p "$DEST_DIR"
install -m 0755 "$BUILD_DIR/$BINARY" "$DEST"
ok "installed $DEST"

# ---------------------------------------------------------------------------
# Step 4: verify
# ---------------------------------------------------------------------------

if [ "$VERIFY" = "yes" ]; then
	step "verifying"
	if out="$(env -i PATH="$PATH" "$DEST" version 2>&1)"; then
		ok "$out"
	else
		warn "could not run '$DEST version': $out"
	fi
fi

# ---------------------------------------------------------------------------
# Step 5: print post-install hints (no shell mutation — that's the
# user's choice).
# ---------------------------------------------------------------------------

path_contains() {
	case ":$1:" in
	*":$2:"*) return 0 ;;
	*)        return 1 ;;
	esac
}

printf '\n'
if ! path_contains "${PATH:-}" "$DEST_DIR"; then
	printf '%sNOTE:%s %s is not on your PATH.\n' "$C_WARN" "$C_RESET" "$DEST_DIR"
	printf '  Add it to your shell profile, e.g.:\n'
	printf '    %sexport PATH="%s:$PATH"%s\n' "$C_DIM" "$DEST_DIR" "$C_RESET"
	printf '\n'
fi

# Make the binary available in this process for the optional `init` step.
export PATH="$DEST_DIR:$PATH"

# ---------------------------------------------------------------------------
# Step 6: optional `mole init`
# ---------------------------------------------------------------------------

if [ "$RUN_INIT" = "yes" ]; then
	# When stdin is a TTY, run interactively. When it's redirected
	# (the typical `curl | sh` case), run non-interactively so the
	# install is scriptable via the MOLE_* env vars documented in
	# `mole init -h`.
	if [ -t 0 ]; then
		step "running mole init (interactive)"
		exec "$DEST" init
	else
		step "running mole init (non-interactive; using MOLE_* env vars)"
		exec "$DEST" init -no-prompt
	fi
fi

step "done"
printf '\n  %s%smole%s %sinstalled successfully%s\n' \
	"$C_BOLD" "$C_MAGENTA" "$C_RESET" "$C_GREEN" "$C_RESET"
printf '  %s\n' "$C_DIM─────────────────────────────────────────────────$C_RESET"
printf '  %s%s%s%s\n' "$C_DIM" "binary     " "$C_RESET" "$DEST"
printf '  %s%s%s%s   (interactive, run once per machine)\n' "$C_DIM" "configure  " "$C_RESET" "${C_GREEN}mole init${C_RESET}"
printf '  %s%s%s%s      (uses ./mole.yaml by default)\n' "$C_DIM" "start      " "$C_RESET" "${C_GREEN}mole up${C_RESET}"
