#!/usr/bin/env bash
# scripts/uninstall.sh — remove a previously installed mole binary
#
# Removes the binary that scripts/install.sh would have placed. Tries
# a small list of common locations and exits 0 even if the binary is
# not present, so this is safe to run repeatedly (and in CI).
#
# It also removes the login service `mole service install` sets up (a
# launchd agent on macOS, a systemd user unit on Linux), which would
# otherwise try to start the removed binary at every login. Run through
# sudo, it removes the service of the user who ran sudo.
#
# Usage:
#   ./scripts/uninstall.sh                  # default locations
#   ./scripts/uninstall.sh --prefix /opt    # remove <prefix>/bin/mole
#   INSTALL_DIR=/custom/path/mole ./scripts/uninstall.sh
#
# Exit codes:
#   0  binary (and login service) removed, or weren't there
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
# Login service (`mole service install`)
#
# Removed before the binary, so the binary itself can do it: `mole service
# uninstall` stops the running service, unloads it and deletes its file.
# Without a mole that knows `service` (an older one, or none left), the same
# steps are done here. `mole service` exists on macOS and Linux only.
# ---------------------------------------------------------------------------

# user_home prints the home directory of user $1, or nothing.
user_home() {
	h=""
	if command -v getent >/dev/null 2>&1; then
		h=$(getent passwd "$1" 2>/dev/null | cut -d: -f6) || h=""
	fi
	if [ -z "$h" ] && command -v dscl >/dev/null 2>&1; then
		h=$(dscl . -read "/Users/$1" NFSHomeDirectory 2>/dev/null | sed -n 's/^NFSHomeDirectory: *//p') || h=""
	fi
	printf '%s\n' "$h"
}

remove_service() {
	os=$(uname -s)
	case "$os" in
	Darwin | Linux) ;;
	*) return 0 ;;
	esac

	# Whose service: ours, or under sudo that of the user who ran sudo.
	svc_as=""
	svc_uid=$(id -u)
	svc_home="${HOME:-}"
	if [ "$svc_uid" = "0" ] && [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
		svc_as="$SUDO_USER"
		svc_uid=$(id -u "$SUDO_USER") || return 0
		svc_home=$(user_home "$SUDO_USER")
	fi
	[ -n "$svc_home" ] || return 0

	if [ "$os" = "Darwin" ]; then
		svc_file="$svc_home/Library/LaunchAgents/dev.luqueee.mole.plist"
	elif [ -n "$svc_as" ]; then
		svc_dir="$svc_home/.config/systemd/user"
		svc_file="$svc_dir/mole.service"
	else
		svc_dir="${XDG_CONFIG_HOME:-$svc_home/.config}/systemd/user"
		svc_file="$svc_dir/mole.service"
	fi
	[ -e "$svc_file" ] || return 0
	echo ">> removing the login service $svc_file"

	if [ -z "$svc_as" ]; then
		# The first mole binary found: any version with `mole service` does.
		svc_bin=""
		for path in "$@"; do
			if [ -n "$path" ] && [ "${path##*/}" = "mole" ] && [ -f "$path" ] && [ -x "$path" ]; then
				svc_bin="$path"
				break
			fi
		done
		[ -n "$svc_bin" ] || svc_bin=$(command -v mole 2>/dev/null) || svc_bin=""
		if [ -n "$svc_bin" ] && out=$("$svc_bin" service uninstall 2>&1); then
			[ -z "$out" ] || printf '   %s\n' "$out"
			return 0
		fi
	fi

	# Stop and unload it, then delete the file (what `mole service uninstall`
	# does). Unloading fails harmlessly when it is not loaded.
	if [ "$os" = "Darwin" ]; then
		launchctl bootout "gui/$svc_uid/dev.luqueee.mole" >/dev/null 2>&1 ||
			launchctl unload -w "$svc_file" >/dev/null 2>&1 || true
	else
		set -- systemctl --user
		[ -z "$svc_as" ] || set -- "$@" --machine="$svc_as@"
		"$@" disable --now mole.service >/dev/null 2>&1 || true
		# What disable removes, should it have failed (no user manager).
		rm -f "$svc_dir/default.target.wants/mole.service"
	fi
	if ! rm -f "$svc_file"; then
		echo "error: failed to remove $svc_file" >&2
		exit 1
	fi
	if [ "$os" = "Linux" ]; then
		"$@" daemon-reload >/dev/null 2>&1 || true
	fi
	echo "   removed $svc_file; mole no longer starts at login"
}

remove_service "$@"

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
