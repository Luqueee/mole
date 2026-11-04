#!/usr/bin/env bash
# scripts/install.sh — install mole (Unix: Linux, macOS, FreeBSD).
#
# What this script does:
#   1. Picks what to install. By default that is the latest mole release: the
#      prebuilt archive for this OS and CPU, checked against the release's
#      SHA256SUMS. That needs only curl or wget, tar and sha256sum or shasum
#      (no git, no Go). It builds from source instead inside a mole clone, for
#      MOLE_SRC or MOLE_REPO, for a MOLE_VERSION that is not a vX.Y.Z release
#      tag (a branch, say), and when a release has no archive for this
#      platform (it says so).
#   2. A source build clones https://github.com/Luqueee/mole (unless it is a
#      local clone) and runs `go build -trimpath`. If Go is missing or too old,
#      it downloads the version the source needs into ~/.cache/mole/go
#      (MOLE_NO_GO_DOWNLOAD=1 refuses to).
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
#   curl -fsSL .../install.sh | sh             # one-liner: the latest release
#   ./scripts/install.sh                       # from a clone: builds it
#   ./scripts/install.sh --prefix /opt         # custom prefix
#   INSTALL_DIR=~/bin/mole ./scripts/install.sh
#
# Options:
#   --prefix <dir>      install under <dir>/bin/mole
#   --no-verify         skip the post-install version check
#   --init              also run `mole init` after install
#   -h, --help          show help

set -eu
# pipefail is not POSIX: dash (the `sh` of Debian and Ubuntu) rejects it, which
# broke `curl ... | sh` there. Use it where it exists.
# shellcheck disable=SC3040
(set -o pipefail) 2>/dev/null && set -o pipefail

# Resolve a filesystem path to this script, when there is one. Used
# only to detect "am I running from inside a clone" (script_dir/.. is
# the repo). In a piped invocation (`curl ... | sh`) there is no file
# behind the script and SELF stays empty — that's fine, the installer
# then installs a release (or clones, for a source build).
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

# MOLE_REPO points at another remote (a fork, or a local repository to try the
# installer against) and always means a source build. MOLE_VERSION is a release
# tag (vX.Y.Z: its archive is installed) or any other git ref (built).
GITHUB_REPO="Luqueee/mole"
REPO="${MOLE_REPO:-https://github.com/$GITHUB_REPO.git}"
BINARY="mole"
DEFAULT_REF="main"
VERSION_REF="${MOLE_VERSION:-$DEFAULT_REF}"
# A ref asked for by name must exist: never fall back to the default branch.
STRICT_REF="no"
[ -n "${MOLE_VERSION:-}" ] && STRICT_REF="yes"

# Where releases are looked up and downloaded from. Tests (or a mirror) point
# these elsewhere, file:// included; GITHUB_TOKEN only ever goes to the real API.
DEFAULT_API_URL="https://api.github.com/repos/$GITHUB_REPO"
API_URL="${MOLE_API_URL:-$DEFAULT_API_URL}"
API_URL="${API_URL%/}"
RELEASE_URL="${MOLE_RELEASE_URL:-https://github.com/$GITHUB_REPO/releases/download}"
RELEASE_URL="${RELEASE_URL%/}"
# Build provenance: auto checks it with `gh attestation verify` when gh is
# installed and signed in (a failure is only a warning), require insists on it,
# off skips it. The SHA-256 check against SHA256SUMS is always made.
ATTESTATION="${MOLE_ATTESTATION:-auto}"

# The ref ends up in a `git clone --branch`, a URL and a linker flag: keep it plain.
if [ -n "${MOLE_VERSION:-}" ]; then
	case "$VERSION_REF" in
	-*|*..*|*[!A-Za-z0-9._/@+-]*)
		echo "error: invalid MOLE_VERSION '$VERSION_REF' (letters, digits and . _ / @ + - only)" >&2
		exit 1
		;;
	esac
fi
case "$ATTESTATION" in
auto|require|off) ;;
*)
	echo "error: invalid MOLE_ATTESTATION '$ATTESTATION' (auto, require or off)" >&2
	exit 1
	;;
esac

# Colour setup — only when stderr is a TTY and NO_COLOR is unset.
if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
	C_STEP=$(printf '\033[1;38;2;125;133;144m')   # bold dim label (section headers)
	C_OK=$(printf '\033[38;2;63;185;80m')          # FORWARD green
	C_WARN=$(printf '\033[38;2;210;153;34m')       # WARN amber
	C_ERR=$(printf '\033[38;2;248;81;73m')         # ERROR red
	C_DIM=$(printf '\033[38;2;110;114;125m')       # timestamp dim
	C_BOLD=$(printf '\033[1m')                      # bold style
	C_BRIGHT=$(printf '\033[38;2;230;237;243m')    # bright message
	C_VALUE=$(printf '\033[38;2;173;186;199m')     # attr value
	C_RESET=$(printf '\033[0m')
else
	C_STEP="" C_OK="" C_WARN="" C_ERR="" C_DIM="" C_BOLD="" C_BRIGHT="" C_VALUE="" C_RESET=""
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
  1. Installs the latest mole release by default: the prebuilt archive for
     this OS and CPU, checked against the release's SHA256SUMS. That needs
     curl or wget, tar, and sha256sum or shasum; no git or Go. Inside a mole
     clone, for MOLE_SRC or MOLE_REPO, for a MOLE_VERSION that is not a
     vX.Y.Z release tag, and when a release has no archive for this platform,
     it builds from source instead (git and Go 1.26.10+).
  2. A source build runs `go build -trimpath`. If Go is missing or too old,
     it downloads the version the source needs (checked against go.dev's
     checksum) into ~/.cache/mole/go; MOLE_NO_GO_DOWNLOAD=1 refuses to.
  3. Copies it to the right place (root -> /usr/local/bin, otherwise
     $HOME/.local/bin). Override with --prefix or INSTALL_DIR.
  4. Verifies with `mole version`.
  5. Prints a hint to add the install dir to PATH if it isn't yet,
     and to run `mole init` to configure.

Usage:
  curl -fsSL .../install.sh | sh             # one-liner: the latest release
  ./scripts/install.sh                       # from a clone: builds it
  ./scripts/install.sh --prefix /opt         # custom prefix
  INSTALL_DIR=~/bin/mole ./scripts/install.sh
  curl -fsSL .../install.sh | MOLE_VERSION=v0.6.1 sh   # a given release
  curl -fsSL .../install.sh | MOLE_VERSION=feat/x sh   # build a branch

Environment:
  MOLE_VERSION       a release tag (vX.Y.Z) to install, or a git ref (branch
                     or tag) to build; a ref that does not exist is an error
  MOLE_REPO          git remote to build from (a fork, or a local repository)
  MOLE_SRC           local clone to build from
  INSTALL_DIR        full path of the installed binary
  GITHUB_TOKEN       sent to api.github.com when looking up the latest release
  MOLE_ATTESTATION   auto (default), require or off: check the archive's build
                     provenance with `gh attestation verify`
  MOLE_RELEASE_ONLY  ignore a mole clone in the working directory

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
# Helpers
# ---------------------------------------------------------------------------

# One scratch directory for downloads and clones, removed on the way out.
TMP_ROOT=""
ensure_tmp() {
	[ -n "$TMP_ROOT" ] || TMP_ROOT="$(mktemp -d)"
}
cleanup() {
	[ -n "$TMP_ROOT" ] && [ -d "$TMP_ROOT" ] && rm -rf "$TMP_ROOT"
	return 0
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

is_mole_repo() {
	local d="$1"
	[ -f "$d/go.mod" ] || return 1
	grep -q "github.com/Luqueee/mole" "$d/go.mod" 2>/dev/null || return 1
	[ -d "$d/cmd/mole" ] || return 1
}

# is_release_tag REF: REF names a CLI release (vX.Y.Z), whose archive can be
# downloaded instead of built.
is_release_tag() {
	printf '%s\n' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'
}

# version_ge A B: dotted version A is at least B (1.26.10 >= 1.26, 1.25.0 < 1.26.10).
version_ge() {
	awk -v a="$1" -v b="$2" 'BEGIN {
		na = split(a, x, "."); nb = split(b, y, ".")
		for (i = 1; i <= 3; i++) {
			p = (i <= na) ? x[i] + 0 : 0; q = (i <= nb) ? y[i] + 0 : 0
			if (p != q) exit (p > q) ? 0 : 1
		}
		exit 0
	}'
}

fetch_to() { # fetch_to URL FILE
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		return 1
	fi
}

# http_get URL FILE [api]: download URL into FILE with curl or wget. Returns 0
# on success, 4 when there is no such file (HTTP 404, or a missing file://
# path) and 1 for any other failure. With "api" it asks for the GitHub API's
# JSON and sends GITHUB_TOKEN, but only to the real API: a MOLE_API_URL that
# points elsewhere never sees the token. The token travels in a config read
# from stdin (curl) or a private wgetrc, never on a command line that other
# users could read with ps.
http_get() {
	local url="$1" out="$2" api="${3:-}" token="" code="" rc=0 hdr wrc
	rm -f "$out"
	if [ "$api" = api ] && [ -n "${GITHUB_TOKEN:-}" ] && [ "$API_URL" = "$DEFAULT_API_URL" ]; then
		token="$GITHUB_TOKEN"
	fi
	if command -v curl >/dev/null 2>&1; then
		code="$(
			{
				[ -n "$token" ] && printf 'header = "Authorization: Bearer %s"\n' "$token"
				[ "$api" = api ] && printf 'header = "Accept: application/vnd.github+json"\n'
				true
			} | curl -sSL --retry 2 -K - -w '%{http_code}' -o "$out" "$url"
		)" || rc=$?
		case "$rc:$code" in
		0:2??|0:000) return 0 ;; # 000: a file:// URL
		0:404|37:*) return 4 ;;  # 37: a file:// path that does not exist
		0:*) echo "  HTTP $code from $url" >&2 ;;
		esac
		return 1
	fi
	if command -v wget >/dev/null 2>&1; then
		hdr="$out.headers"
		wrc="$out.wgetrc"
		(
			umask 077
			{
				[ -n "$token" ] && printf 'header = Authorization: Bearer %s\n' "$token"
				[ "$api" = api ] && printf 'header = Accept: application/vnd.github+json\n'
				true
			} >"$wrc"
		)
		WGETRC="$wrc" wget -q -S -O "$out" "$url" 2>"$hdr" || rc=$?
		code="$(awk '$1 ~ /^HTTP\// { c = $2 } END { print c }' "$hdr" 2>/dev/null || true)"
		rm -f "$hdr" "$wrc"
		[ "$rc" -eq 0 ] && return 0
		[ "$code" = 404 ] && return 4
		[ -n "$code" ] && echo "  HTTP $code from $url" >&2
		return 1
	fi
	die "neither curl nor wget is installed; install one of them and re-run"
}

# sha256_of FILE: its SHA-256, in lowercase hex.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print tolower($1) }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print tolower($1) }'
	elif command -v sha256 >/dev/null 2>&1; then
		sha256 -q "$1" | tr 'A-F' 'a-f' # FreeBSD
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | awk '{ print tolower($NF) }'
	else
		return 1
	fi
}

# ---------------------------------------------------------------------------
# Step 1: decide what to install
# ---------------------------------------------------------------------------

MODE=""         # "release" (a prebuilt archive) or "source" (a build)
PROJECT_ROOT="" # the source to build, once known
CLONED="no"
REL_TAG=""      # the release to install, e.g. v0.6.1
REL_OS=""
REL_ARCH=""
REL_MISSING=""  # why the release could not be used, when it could not
BIN=""          # the binary to install, once there is one

# Use the clone this script was run from, when there is one.
resolve_from_script_dir() {
	local script_dir=""
	# $SELF was resolved at the top of the script and is empty in a
	# piped invocation, so there is nothing to look at then.
	if [ -n "$SELF" ] && [ -f "$SELF" ]; then
		script_dir="$(cd "$(dirname "$SELF")" && pwd 2>/dev/null || true)"
	fi
	if [ -n "$script_dir" ] && is_mole_repo "$script_dir/.."; then
		PROJECT_ROOT="$(cd "$script_dir/.." && pwd)"; return 0
	fi
	return 1
}

# A mole clone around us: the working directory, or the one this script is in.
resolve_local_checkout() {
	if is_mole_repo "$PWD"; then
		PROJECT_ROOT="$PWD"; return 0
	fi
	resolve_from_script_dir
}

choose_mode() {
	if [ -n "${MOLE_SRC:-}" ]; then
		is_mole_repo "$MOLE_SRC" || die "MOLE_SRC is not a mole source directory: $MOLE_SRC"
		PROJECT_ROOT="$MOLE_SRC"
		MODE="source"
		return 0
	fi
	# A version asked for by name means that version, not whatever checkout
	# happens to be here; `mole update` sets MOLE_RELEASE_ONLY for the same reason.
	if [ -z "${MOLE_VERSION:-}" ] && [ -z "${MOLE_RELEASE_ONLY:-}" ] && resolve_local_checkout; then
		MODE="source"
		return 0
	fi
	# Releases come from Luqueee/mole only: another remote is always built.
	if [ -n "${MOLE_REPO:-}" ]; then
		MODE="source"
		return 0
	fi
	if [ -n "${MOLE_VERSION:-}" ] && ! is_release_tag "$MOLE_VERSION"; then
		MODE="source"
		return 0
	fi
	MODE="release"
}

# release_tags FILE: the versions (without the v) of the CLI releases in a page
# of GitHub's release list that are published (no drafts, no prereleases) and
# tagged vX.Y.Z; the desktop app's releases (desktop-v*) share the repository
# and its "latest release". Only three fields are read, which every release
# object has exactly once and none of its nested objects (assets, author) has,
# so they come in groups of three, one group per release, and no JSON parser is
# needed. A JSON string cannot hold an unescaped quote, so release notes that
# mention "tag_name" cannot pass for the field.
release_tags() {
	{ grep -oE '"(tag_name|draft|prerelease)"[[:space:]]*:[[:space:]]*("[^"]*"|true|false)' "$1" || true; } |
		awk '
		{
			k = $0; sub(/^"/, "", k); sub(/".*$/, "", k)
			v = $0; sub(/^[^:]*:[ \t]*/, "", v); gsub(/"/, "", v)
			if (k in rec) bad = 1
			rec[k] = v; n++
			if (n == 3) {
				if (rec["draft"] == "false" && rec["prerelease"] == "false" && rec["tag_name"] ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/)
					print substr(rec["tag_name"], 2)
				split("", rec); n = 0
			}
		}
		END { exit (bad || n) ? 1 : 0 }'
}

# Set REL_TAG to the newest published CLI release.
find_latest_release() {
	local page=1 file vers rc msg hint=""
	ensure_tmp
	file="$TMP_ROOT/releases.json"
	[ -n "${GITHUB_TOKEN:-}" ] || hint=" (rate-limited? set GITHUB_TOKEN, or MOLE_VERSION to a release tag)"
	step "looking up the latest mole release"
	while [ "$page" -le 10 ]; do
		rc=0
		http_get "$API_URL/releases?per_page=100&page=$page" "$file" api || rc=$?
		if [ "$rc" -ne 0 ]; then
			msg="$(sed -n 's/.*"message"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$file" 2>/dev/null | head -n 1 || true)"
			die "could not list the releases at $API_URL/releases${msg:+: $msg}$hint"
		fi
		# An empty page: there are no more releases.
		grep -q '"tag_name"' "$file" || break
		vers="$(release_tags "$file")" || die "could not read the release list from $API_URL/releases"
		if [ -n "$vers" ]; then
			REL_TAG="v$(printf '%s\n' "$vers" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)"
			ok "latest release: $REL_TAG"
			return 0
		fi
		page=$((page + 1))
	done
	die "found no published mole release at $API_URL/releases (set MOLE_VERSION to a branch or tag to build it from source)"
}

# Set REL_OS and REL_ARCH to the platform's release archive, or leave them
# empty when releases have none for it (the archives are what
# scripts/release-archives.sh builds).
release_platform() {
	REL_OS=""
	REL_ARCH=""
	case "$(uname -s)" in
	Linux) REL_OS=linux ;;
	Darwin) REL_OS=darwin ;;
	FreeBSD) REL_OS=freebsd ;;
	esac
	case "$(uname -m)" in
	x86_64|amd64) REL_ARCH=amd64 ;;
	aarch64|arm64) REL_ARCH=arm64 ;;
	esac
	# A shell running under Rosetta says x86_64 on an Apple silicon Mac:
	# install the native build.
	if [ "$REL_OS" = darwin ] && [ "$REL_ARCH" = amd64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
		REL_ARCH=arm64
	fi
}

# Check the archive's build provenance with the GitHub CLI (MOLE_ATTESTATION).
verify_attestation() { # verify_attestation FILE
	local name out
	name="$(basename "$1")"
	case "$ATTESTATION" in
	off) return 0 ;;
	auto)
		command -v gh >/dev/null 2>&1 || return 0
		gh auth status >/dev/null 2>&1 || return 0
		;;
	require)
		command -v gh >/dev/null 2>&1 || die "MOLE_ATTESTATION=require needs the GitHub CLI (gh) to check $name"
		;;
	esac
	if out="$(gh attestation verify "$1" --repo "$GITHUB_REPO" 2>&1)"; then
		ok "verified the build provenance of $name (gh attestation verify)"
	elif [ "$ATTESTATION" = require ]; then
		die "gh attestation verify could not verify $name: $(printf '%s\n' "$out" | tail -n 3 | tr '\n' ' ')"
	else
		warn "gh could not verify the build provenance of $name (older releases have none); relying on its SHA-256"
	fi
}

# Download REL_TAG's archive for this platform, check it against the release's
# SHA256SUMS and unpack it: BIN is then the binary. A release without that
# archive sets REL_MISSING instead; anything else that goes wrong (a checksum
# that does not match, no SHA256SUMS or no line for the archive in it, a failed
# download) stops the install.
download_release() {
	local ver file dir want got rc
	ver="${REL_TAG#v}"
	file="mole_${ver}_${REL_OS}_${REL_ARCH}.tar.gz"
	ensure_tmp
	dir="$TMP_ROOT/release"
	rm -rf "$dir"
	mkdir -p "$dir/x"
	step "downloading $file ($REL_TAG)"
	rc=0
	http_get "$RELEASE_URL/$REL_TAG/$file" "$dir/$file" || rc=$?
	if [ "$rc" -eq 4 ]; then
		REL_MISSING="release $REL_TAG has no $file"
		return 0
	fi
	[ "$rc" -eq 0 ] || die "could not download $RELEASE_URL/$REL_TAG/$file"
	rc=0
	http_get "$RELEASE_URL/$REL_TAG/SHA256SUMS" "$dir/SHA256SUMS" || rc=$?
	[ "$rc" -eq 0 ] || die "could not download the SHA256SUMS of $REL_TAG; refusing to install an archive it cannot check"
	want="$(awk -v f="$file" '$2 == f || $2 == "*" f { print tolower($1); exit }' "$dir/SHA256SUMS")"
	case "$want" in
	*[!0-9a-f]*|"") die "the SHA256SUMS of $REL_TAG has no checksum for $file; refusing to install it" ;;
	esac
	[ "${#want}" -eq 64 ] || die "the SHA256SUMS of $REL_TAG has no valid checksum for $file; refusing to install it"
	got="$(sha256_of "$dir/$file")" || die "no sha256sum, shasum, sha256 or openssl to check $file with"
	if [ "$got" != "$want" ]; then
		die "$file does not match its checksum in the release's SHA256SUMS (got $got, want $want); not installing it"
	fi
	ok "verified the SHA-256 of $file"
	verify_attestation "$dir/$file"
	tar -xzf "$dir/$file" -C "$dir/x" "$BINARY" || die "could not unpack $BINARY from $file"
	if [ ! -f "$dir/x/$BINARY" ] || [ -h "$dir/x/$BINARY" ]; then
		die "$file holds no $BINARY binary"
	fi
	BIN="$dir/x/$BINARY"
}

# A release that cannot be installed as an archive is built from its tag.
fall_back_to_source() { # fall_back_to_source REASON
	warn "$1: building $REL_TAG from source instead (needs git, and Go is fetched when missing)"
	VERSION_REF="$REL_TAG"
	STRICT_REF="yes"
	MODE="source"
}

install_release() {
	REL_TAG="${MOLE_VERSION:-}"
	[ -n "$REL_TAG" ] || find_latest_release
	release_platform
	if [ -z "$REL_OS" ] || [ -z "$REL_ARCH" ]; then
		fall_back_to_source "mole releases have no archive for $(uname -s)/$(uname -m)"
		return 0
	fi
	download_release
	if [ -n "$REL_MISSING" ]; then
		fall_back_to_source "$REL_MISSING"
	fi
}

# ---------------------------------------------------------------------------
# Step 2: build from source, when that is what is installed
# ---------------------------------------------------------------------------

clone_ref() {
	local hint
	ensure_tmp
	step "cloning $REPO (ref: $VERSION_REF) into $TMP_ROOT/mole"
	if ! git clone --depth 1 --branch "$VERSION_REF" "$REPO" "$TMP_ROOT/mole" >/dev/null 2>&1; then
		# A ref asked for by name must be the one installed: falling back to
		# main would hand someone testing a branch the wrong code, silently.
		if [ "$STRICT_REF" = "yes" ] && [ "$VERSION_REF" != "$DEFAULT_REF" ]; then
			hint="$(git ls-remote --heads --tags "$REPO" 2>/dev/null | sed 's#.*refs/\(heads\|tags\)/##' | grep -iF -- "${VERSION_REF##*/}" | head -5 | tr '\n' ' ' || true)"
			die "ref '$VERSION_REF' not found in $REPO${hint:+ (similar: $hint)}"
		fi
		warn "ref '$VERSION_REF' not found, falling back to the default branch"
		rm -rf "$TMP_ROOT/mole"
		git clone --depth 1 "$REPO" "$TMP_ROOT/mole" >/dev/null
	fi
	CLONED="yes"
	PROJECT_ROOT="$TMP_ROOT/mole"
}

# The Go version the source asks for (the `go` line of go.mod).
go_wanted() {
	awk '$1 == "go" { print $2; exit }' "$PROJECT_ROOT/go.mod" 2>/dev/null
}

# download_go VERSION: fetch the official Go toolchain into a cache under the
# user's home (nothing system-wide is touched) and print the path of its `go`.
# The archive is checked against the digest published next to it.
download_go() {
	local ver="$1" os arch cache dest tmp base file want got
	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	FreeBSD) os=freebsd ;;
	*) die "cannot fetch Go for $(uname -s): install Go $ver+ from https://go.dev/dl/ and re-run" ;;
	esac
	case "$(uname -m)" in
	x86_64|amd64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	armv6*|armv7*) arch=armv6l ;;
	i386|i686) arch=386 ;;
	*) die "cannot fetch Go for $(uname -m): install Go $ver+ from https://go.dev/dl/ and re-run" ;;
	esac
	cache="${XDG_CACHE_HOME:-$HOME/.cache}/mole/go"
	dest="$cache/$ver"
	if [ -x "$dest/go/bin/go" ]; then
		echo "$dest/go/bin/go"
		return 0
	fi
	base="${MOLE_GO_URL:-https://dl.google.com/go}"
	file="go$ver.$os-$arch.tar.gz"
	mkdir -p "$cache"
	tmp="$(mktemp -d "$cache/.dl.XXXXXX")"
	step "downloading Go $ver ($os/$arch) into $dest" >&2
	fetch_to "$base/$file" "$tmp/$file" || { rm -rf "$tmp"; die "could not download $base/$file (set MOLE_NO_GO_DOWNLOAD=1 and install Go yourself, or MOLE_GO_URL to a mirror)"; }
	want="$(fetch_to "$base/$file.sha256" "$tmp/sum" && tr -d ' \n\r' <"$tmp/sum" || true)"
	got="$(sha256_of "$tmp/$file" || true)"
	case "$want" in
	*[!0-9a-f]*|"") rm -rf "$tmp"; die "could not read the published checksum of $file; refusing to run an unverified Go" ;;
	esac
	if [ "$want" != "$got" ]; then
		rm -rf "$tmp"
		die "the Go archive does not match its published checksum (got $got, want $want)"
	fi
	mkdir -p "$tmp/x"
	tar -xzf "$tmp/$file" -C "$tmp/x" || { rm -rf "$tmp"; die "could not unpack $file"; }
	[ -x "$tmp/x/go/bin/go" ] || { rm -rf "$tmp"; die "the Go archive has no go/bin/go"; }
	rm -rf "$dest"
	mkdir -p "$dest"
	mv "$tmp/x/go" "$dest/go"
	rm -rf "$tmp"
	echo "$dest/go/bin/go"
}

build_from_source() {
	if [ -z "$PROJECT_ROOT" ]; then
		command -v git >/dev/null 2>&1 || die "building from source needs 'git' to clone $REPO (and Go, fetched when missing)"
		clone_ref
	fi
	step "using source: $PROJECT_ROOT"

	# Pick the Go to build with: the one on PATH when it is new enough, else a
	# private copy of the version go.mod asks for (so `curl | sh` works on a
	# machine without Go, or with an older one).
	GO_BIN="${GO:-$(command -v go || true)}"
	GO_WANT="$(go_wanted)"
	GO_HAVE=""
	# Asked from inside the source, so a Go that can fetch the toolchain go.mod
	# names by itself (1.21+) counts as new enough.
	[ -n "$GO_BIN" ] && GO_HAVE="$(cd "$PROJECT_ROOT" && "$GO_BIN" env GOVERSION 2>/dev/null | sed 's/^go//' || true)"
	if [ -z "${GO:-}" ] && [ -n "$GO_WANT" ] && { [ -z "$GO_HAVE" ] || ! version_ge "$GO_HAVE" "$GO_WANT"; }; then
		if [ -n "${MOLE_NO_GO_DOWNLOAD:-}" ]; then
			die "Go $GO_WANT+ is required (found ${GO_HAVE:-none}); install it from https://go.dev/dl/ and re-run"
		fi
		if [ -n "$GO_HAVE" ]; then
			step "Go $GO_HAVE is older than the $GO_WANT this source needs"
		else
			step "Go is not installed"
		fi
		GO_BIN="$(download_go "$GO_WANT")"
		# The private toolchain is exactly the one wanted: never go looking for another.
		export GOTOOLCHAIN=local
		ok "using $GO_BIN"
	fi
	[ -n "$GO_BIN" ] || die "'go' is not installed. Install Go ${GO_WANT:-1.26.10}+ from https://go.dev/dl/ and re-run."

	# Say where the binary comes from, so `mole version` shows the branch and
	# commit that was installed (stamped with -ldflags; empty without git).
	BUILD_REF=""
	BUILD_COMMIT=""
	if command -v git >/dev/null 2>&1 && git -C "$PROJECT_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
		BUILD_COMMIT="$(git -C "$PROJECT_ROOT" rev-parse --short=7 HEAD 2>/dev/null || true)"
		if [ "$CLONED" = "yes" ]; then
			BUILD_REF="$VERSION_REF"
		else
			BUILD_REF="$(git -C "$PROJECT_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
			[ "$BUILD_REF" = "HEAD" ] && BUILD_REF=""
			if [ -n "$(git -C "$PROJECT_ROOT" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
				BUILD_COMMIT="${BUILD_COMMIT}+dirty"
			fi
		fi
	fi
	case "$BUILD_REF" in *[!A-Za-z0-9._/@+-]*) BUILD_REF="" ;; esac

	BUILD_DIR="$PROJECT_ROOT/dist"
	mkdir -p "$BUILD_DIR"
	step "building $BINARY${BUILD_REF:+ from $BUILD_REF}${BUILD_COMMIT:+ ($BUILD_COMMIT)}"
	(
		cd "$PROJECT_ROOT"
		"$GO_BIN" build -trimpath -ldflags "-X main.buildRef=$BUILD_REF -X main.buildCommit=$BUILD_COMMIT" -o "$BUILD_DIR/$BINARY" ./cmd/mole
	)
	[ -f "$BUILD_DIR/$BINARY" ] || die "build did not produce $BUILD_DIR/$BINARY"
	ok "built $BUILD_DIR/$BINARY"
	BIN="$BUILD_DIR/$BINARY"
}

choose_mode
if [ "$MODE" = "release" ]; then
	install_release
fi
if [ "$MODE" = "source" ]; then
	build_from_source
fi
[ -n "$BIN" ] || die "nothing to install"

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
install -m 0755 "$BIN" "$DEST"
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
	# exec replaces this shell, so the EXIT trap would never run.
	cleanup
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
	"$C_BOLD" "$C_BRIGHT" "$C_RESET" "$C_OK" "$C_RESET"
printf '  %s\n' "${C_DIM}─────────────────────────────────────────────────${C_RESET}"
printf '  %s%s%s%s\n' "$C_DIM" "binary     " "$C_RESET" "$DEST"
printf '  %s%s%s%s   (interactive, run once per machine)\n' "$C_DIM" "configure  " "$C_RESET" "${C_VALUE}mole init${C_RESET}"
printf '  %s%s%s%s      (uses ./mole.yaml by default)\n' "$C_DIM" "start      " "$C_RESET" "${C_VALUE}mole up${C_RESET}"
