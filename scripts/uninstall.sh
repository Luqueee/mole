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
# ---------------------------------------------------------------------------

CANDIDATES=()
if [ -n "$PREFIX" ]; then
	CANDIDATES+=("$PREFIX/bin/mole")
else
	# Honour INSTALL_DIR (file, not directory).
	if [ -n "${INSTALL_DIR:-}" ]; then
		CANDIDATES+=("$INSTALL_DIR")
	fi
	# Common install locations, root and user.
	CANDIDATES+=("/usr/local/bin/mole" "/usr/bin/mole")
	if [ -n "${HOME:-}" ]; then
		CANDIDATES+=(
			"$HOME/.local/bin/mole"
			"$HOME/bin/mole"
		)
	fi
fi

# De-dup while preserving order.
declare -A seen=()
UNIQUE=()
for c in "${CANDIDATES[@]}"; do
	[ -z "$c" ] && continue
	if [ -z "${seen[$c]:-}" ]; then
		seen[$c]=1
		UNIQUE+=("$c")
	fi
done

# ---------------------------------------------------------------------------
# Remove
# ---------------------------------------------------------------------------

REMOVED=0
for path in "${UNIQUE[@]}"; do
	if [ -e "$path" ]; then
		# Refuse to rm anything that doesn't look like our binary
		# (name match is good enough; mole is a unique name).
		base="$(basename "$path")"
		if [ "$base" != "mole" ]; then
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
	CONFIG_DIRS=()
	if [ -n "${XDG_CONFIG_HOME:-}" ]; then
		CONFIG_DIRS+=("$XDG_CONFIG_HOME/mole")
	fi
	if [ -n "${HOME:-}" ]; then
		CONFIG_DIRS+=("$HOME/.config/mole")
	fi
	for d in "${CONFIG_DIRS[@]}"; do
		if [ -d "$d" ]; then
			echo ">> purging $d"
			rm -rf "$d"
		fi
	done
fi

echo ">> done."
