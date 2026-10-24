#!/usr/bin/env bash
# scripts/test-install-branch.sh — check that install.sh builds the ref it is
# asked for, says so in `mole version`, and refuses to fall back to another.
#
# It needs no network: MOLE_REPO points the installer at a throwaway copy of
# this repository, with a branch and a tag made up for the test.
#
#   ./scripts/test-install-branch.sh
#
# Exit status 0 when every check passes.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
check() { # check <description> <command...>
	local what="$1"
	shift
	if "$@" >/dev/null 2>&1; then
		pass=$((pass + 1))
		printf '  ok   %s\n' "$what"
	else
		fail=$((fail + 1))
		printf '  FAIL %s\n' "$what"
	fi
}

# version_has <binary> <text>: `<binary> version` mentions <text> (fixed string).
version_has() {
	"$1" version 2>&1 | grep -qF -- "$2"
}

# A repository to install from: this one, plus a branch and a tag of our own.
git clone -q --bare "$ROOT" "$TMP/origin.git"
git -C "$TMP/origin.git" branch try/install-me HEAD
git -C "$TMP/origin.git" tag try-tag HEAD
WANT="$(git -C "$TMP/origin.git" rev-parse --short=7 try/install-me)"

# Run from a folder that is not a mole checkout, so the installer clones.
mkdir "$TMP/cwd"
cd "$TMP/cwd"
install_ref() { # install_ref <ref|""> <dest>
	local ref="$1" dest="$2"
	if [ -n "$ref" ]; then
		env MOLE_REPO="$TMP/origin.git" MOLE_VERSION="$ref" INSTALL_DIR="$dest" NO_COLOR=1 \
			bash "$ROOT/scripts/install.sh" --no-verify
	else
		env MOLE_REPO="$TMP/origin.git" INSTALL_DIR="$dest" NO_COLOR=1 \
			bash "$ROOT/scripts/install.sh" --no-verify
	fi
}

echo "== a branch"
install_ref try/install-me "$TMP/bin-branch/mole" >"$TMP/out-branch.txt" 2>&1 || true
check "the binary is installed" test -x "$TMP/bin-branch/mole"
check "mole version names the branch and commit" version_has "$TMP/bin-branch/mole" "(try/install-me@$WANT)"

echo "== a tag"
install_ref try-tag "$TMP/bin-tag/mole" >"$TMP/out-tag.txt" 2>&1 || true
check "a tag installs" version_has "$TMP/bin-tag/mole" "(try-tag@$WANT)"

echo "== a ref that does not exist"
if install_ref no/such-branch "$TMP/bin-missing/mole" >"$TMP/out-missing.txt" 2>&1; then
	check "the install fails" false
else
	check "the install fails" true
fi
check "nothing was installed in its place" test ! -e "$TMP/bin-missing/mole"
check "the error says which ref was not found" grep -qF "ref 'no/such-branch' not found" "$TMP/out-missing.txt"

echo "== refs that are not plain refs"
rejects() { # rejects <ref>: the installer refuses it
	! env MOLE_REPO="$TMP/origin.git" MOLE_VERSION="$1" INSTALL_DIR="$TMP/bin-bad/mole" NO_COLOR=1 \
		bash "$ROOT/scripts/install.sh" --no-verify
}
for bad in '--upload-pack=touch /tmp/x' 'a b' '../../etc' '-x' 'a;b' 'a$(id)'; do
	check "rejected: $bad" rejects "$bad"
done
check "no binary came from a rejected ref" test ! -e "$TMP/bin-bad/mole"

echo "== no MOLE_VERSION: the default branch, as before"
install_ref "" "$TMP/bin-default/mole" >"$TMP/out-default.txt" 2>&1 || true
check "the default branch installs" test -x "$TMP/bin-default/mole"

echo "== from a clone: the local branch and commit"
git clone -q "$ROOT" "$TMP/clone"
git -C "$TMP/clone" checkout -q -b local-work
(cd "$TMP/clone" && env INSTALL_DIR="$TMP/bin-clone/mole" NO_COLOR=1 bash scripts/install.sh --no-verify) >"$TMP/out-clone.txt" 2>&1 || true
check "a clone's binary names its own branch" \
	version_has "$TMP/bin-clone/mole" "(local-work@$(git -C "$TMP/clone" rev-parse --short=7 HEAD))"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
if [ "$fail" -ne 0 ]; then
	echo "--- installer output of the runs above:" >&2
	tail -n 12 "$TMP"/out-*.txt >&2 || true
	exit 1
fi
