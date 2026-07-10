#!/usr/bin/env bash
# scripts/uninstall.sh — remove a previously installed mole binary
#
# Removes the binary that scripts/install.sh would have placed. Tries
# a small list of common locations and exits 0 even if the binary is
# not present, so this is safe to run repeatedly (and in CI).
#
# Usage:
#   ./scripts/uninstall.sh                  # default locations
#   ./scripts/uninstall.sh --prefix /opt    # remove <prefix>/bin/mole
#   INSTALL_DIR=/custom/path/mole ./scripts/uninstall.sh
#
# Exit codes:
#   0  binary removed (or wasn't there)
#   1  I/O failure
#   2  bad CLI usage

set -euo pipefail

usage() {
	cat <<EOF
Usage: $0 [options]

Options:
  --prefix <dir>   Look for the binary under <prefix>/bin/.
                   Overrides the default candidate list.
  --purge          Also remove \$XDG_CONFIG_HOME/mole/ and
                   \$HOME/.config/mole/ if they exist.
  -h, --help       Show this help and exit.
EOF
}

PREFIX=""
PURGE="false"
while [ $# -gt 0 ]; do
	case "$1" in
	--prefix)
		[ $# -ge 2 ] || { echo "error: --prefix requires a value" >&2; exit 2; }
		PREFIX="$2"
		shift 2
		;;
	--prefix=*)
		PREFIX="${1#--prefix=}"
		shift
		;;
	--purge)
		PURGE="true"
		shift
		;;
	-h|--help)
		usage
		exit 0
		;;
	*)
		echo "error: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

# ---------------------------------------------------------------------------
# Candidate paths to remove
#
# POSIX sh has no arrays, so the candidate list lives in the positional
# parameters ($@). The CLI has already been consumed by the parse loop
# above ($# is 0 here), so overwriting $@ with `set --` is safe.
# ---------------------------------------------------------------------------

if [ -n "$PREFIX" ]; then
	set -- "$PREFIX/bin/mole"
else
	set --
	# Honour INSTALL_DIR (file, not directory).
	if [ -n "${INSTALL_DIR:-}" ]; then
		set -- "$@" "$INSTALL_DIR"
	fi
	# Common install locations, root and user.
	set -- "$@" "/usr/local/bin/mole" "/usr/bin/mole"
	if [ -n "${HOME:-}" ]; then
		set -- "$@" "$HOME/.local/bin/mole" "$HOME/bin/mole"
	fi
fi

# ---------------------------------------------------------------------------
# Remove
#
# `seen` is a newline-delimited set used to de-dup while preserving order
# (INSTALL_DIR may repeat a default path). Install paths never contain
# newlines, so newline framing is an unambiguous membership test.
# ---------------------------------------------------------------------------

REMOVED=0
seen="
"
for path in "$@"; do
	[ -z "$path" ] && continue
	case "$seen" in
	*"
$path
"*) continue ;;
	esac
	seen="$seen$path
"
	if [ -e "$path" ]; then
		# Refuse to rm anything that doesn't look like our binary
		# (name match is good enough; mole is a unique name).
		if [ "${path##*/}" != "mole" ]; then
			echo "skip: $path (name does not match 'mole')" >&2
			continue
		fi
		echo ">> removing $path"
		if rm -f "$path"; then
			REMOVED=$((REMOVED + 1))
		else
			echo "error: failed to remove $path" >&2
			exit 1
		fi
	fi
done

if [ "$REMOVED" -eq 0 ]; then
	echo ">> mole was not installed in any of the default locations."
else
	echo ">> removed $REMOVED file(s)."
fi

# ---------------------------------------------------------------------------
# Optional config purge
# ---------------------------------------------------------------------------

if [ "$PURGE" = "true" ]; then
	set --
	if [ -n "${XDG_CONFIG_HOME:-}" ]; then
		set -- "$@" "$XDG_CONFIG_HOME/mole"
	fi
	if [ -n "${HOME:-}" ]; then
		set -- "$@" "$HOME/.config/mole"
	fi
	for d in "$@"; do
		if [ -d "$d" ]; then
			echo ">> purging $d"
			rm -rf "$d"
		fi
	done
fi

echo ">> done."
