#!/usr/bin/env bash
# scripts/test-fixture.sh — prepare a clean scratch environment for
# exercising scripts/install.sh end-to-end.
#
# The install script is fully self-contained: when invoked from any
# working directory with a path to scripts/install.sh, it discovers
# the mole source by inspecting the script's own location (the parent
# of scripts/ in this repo) and installs the built binary into
# --prefix. So the fixture only needs to provide:
#
#   1. an empty scratch CWD that does NOT look like a mole repo
#      (so we exercise the "find source via install.sh path" branch
#      rather than the "we are inside a mole repo" shortcut), and
#   2. a fresh, empty --prefix directory so prior runs cannot
#      mask install failures.
#
# The fixture does NOT run the installer itself; that is left to the
# caller so the test can pass arguments (--prefix, --init, etc.) and
# observe the result. Use:
#
#   ./scripts/test-fixture.sh            # create the fixture
#   ./scripts/test-fixture.sh clean      # remove the fixture + install
#   ./scripts/test-fixture.sh --help
#
# Environment overrides:
#   FIXTURE_DIR     (default: /tmp/mole-cfg-test)
#   INSTALL_PREFIX  (default: /tmp/mole-prefix)
#   REPO_ROOT       (default: parent of this script's directory)
#
# After running the fixture, the canonical test invocation is:
#
#   cd "$FIXTURE_DIR"
#   bash "$REPO_ROOT/scripts/install.sh" --prefix "$INSTALL_PREFIX"
#
# Add --init to also exercise the post-install "mole init" step
# (interactive on a TTY, or -no-prompt when stdin is not a TTY).
#
# Exit codes:
#   0  success
#   1  I/O failure
#   2  bad CLI usage
#
# This is a host fixture, not a Go unit test — it leaves no files
# inside the repo. /tmp paths are the convention here; on macOS that
# is /tmp (a symlink to /private/tmp) and on Linux it is a real tmpfs.

set -euo pipefail

SELF="${BASH_SOURCE[0]:-$0}"
REPO_ROOT_DEFAULT="$(cd "$(dirname "$SELF")/.." && pwd)"
FIXTURE_DIR="${FIXTURE_DIR:-/tmp/mole-cfg-test}"
INSTALL_PREFIX="${INSTALL_PREFIX:-/tmp/mole-prefix}"
REPO_ROOT="${REPO_ROOT:-$REPO_ROOT_DEFAULT}"

usage() {
	sed -n '2,/^set -euo pipefail/p' "$SELF" | sed 's/^# \{0,1\}//' | sed '$d'
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

cmd="${1:-setup}"

if [ "$cmd" = "-h" ] || [ "$cmd" = "--help" ]; then
	usage
	exit 0
fi

# Validate the repo root early so any subsequent error is unambiguous.
[ -f "$REPO_ROOT/go.mod" ] && [ -d "$REPO_ROOT/cmd/mole" ] \
	|| die "REPO_ROOT does not look like a mole repo: $REPO_ROOT (set REPO_ROOT to override)"

case "$cmd" in
setup)
	# mkdir -p is safe to re-run; we just need the dir to exist and
	# to be empty enough to be a credible scratch CWD. Leave any
	# prior artifacts (e.g. mole.yaml from a previous --init run) in
	# place so the test can also exercise the "config already exists"
	# branch of the installer — that is the whole point of a fixture
	# you can poke at. Just make sure the dir exists.
	mkdir -p "$FIXTURE_DIR"

	# The install prefix must be a clean directory: a stale mole
	# binary from a previous run would let `mole version` succeed
	# and mask a broken install. Wipe it.
	rm -rf "$INSTALL_PREFIX"
	mkdir -p "$INSTALL_PREFIX"

	# A small marker so anyone landing in the dir later can see what
	# it is. The marker carries the exact commands to drive the test
	# so a re-run of the fixture (or a fresh shell) is self-describing.
	cat > "$FIXTURE_DIR/.fixture-info" <<EOF
# Test fixture created by scripts/test-fixture.sh
# (this file is informational; safe to delete)

repo root:     $REPO_ROOT
fixture dir:   $FIXTURE_DIR
install prefix: $INSTALL_PREFIX

# Exercise the install (no --init):
cd $FIXTURE_DIR
bash $REPO_ROOT/scripts/install.sh --prefix $INSTALL_PREFIX

# Or with --init (interactive; needs a TTY):
cd $FIXTURE_DIR
bash $REPO_ROOT/scripts/install.sh --prefix $INSTALL_PREFIX --init

# Or --init non-interactively (stdin not a TTY → -no-prompt + env vars):
cd $FIXTURE_DIR
MOLE_REMOTE=dev@host MOLE_PORTS=3000,5173 \\
    bash $REPO_ROOT/scripts/install.sh --prefix $INSTALL_PREFIX --init < /dev/null

# Tear down:
$REPO_ROOT/scripts/test-fixture.sh clean
EOF

	printf '== test fixture ready\n'
	printf '   cwd:         %s\n' "$FIXTURE_DIR"
	printf '   install to:  %s\n' "$INSTALL_PREFIX"
	printf '   repo root:   %s\n' "$REPO_ROOT"
	printf '   marker file: %s/.fixture-info\n' "$FIXTURE_DIR"
	printf '\n'
	printf 'See %s/.fixture-info for the exact commands.\n' "$FIXTURE_DIR"
	;;
clean)
	# Remove both the scratch CWD and the install destination. Safe
	# to run repeatedly.
	rm -rf "$FIXTURE_DIR" "$INSTALL_PREFIX"
	printf '== removed %s and %s\n' "$FIXTURE_DIR" "$INSTALL_PREFIX"
	;;
*)
	printf 'error: unknown command: %s (try --help)\n' "$cmd" >&2
	exit 2
	;;
esac
