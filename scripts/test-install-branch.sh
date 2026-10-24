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
if install_ref try/install-me "$TMP/bin-branch/mole" >"$TMP/out-branch.txt" 2>&1; then
	check "the installer exits 0 after installing" true
else
	check "the installer exits 0 after installing" false
fi
check "the binary is installed" test -x "$TMP/bin-branch/mole"
check "mole version names the branch and commit" version_has "$TMP/bin-branch/mole" "(try/install-me@$WANT)"

echo "== under a POSIX sh (dash on Debian and Ubuntu), as in curl ... | sh"
if env MOLE_REPO="$TMP/origin.git" MOLE_VERSION=try/install-me INSTALL_DIR="$TMP/bin-sh/mole" NO_COLOR=1 \
	sh "$ROOT/scripts/install.sh" --no-verify >"$TMP/out-sh.txt" 2>&1; then
	check "sh installs and exits 0" true
else
	check "sh installs and exits 0" false
fi
check "and the result is the branch build" version_has "$TMP/bin-sh/mole" "(try/install-me@$WANT)"

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

echo "== mole update -branch replaces the running binary with the branch"
# The installers are fetched from MOLE_RAW_URL/refs/heads/<branch>/scripts/.
mkdir -p "$TMP/raw/refs/heads/try/install-me/scripts"
cp "$ROOT/scripts/install.sh" "$TMP/raw/refs/heads/try/install-me/scripts/install.sh"
(cd "$ROOT" && go build -o "$TMP/self/mole" ./cmd/mole)
check "the binary starts without a branch label" bash -c '! "$1" version | grep -qF "try/install-me"' _ "$TMP/self/mole"
update_env=(MOLE_RAW_URL="file://$TMP/raw" MOLE_REPO="$TMP/origin.git" NO_COLOR=1)
if env "${update_env[@]}" "$TMP/self/mole" update -branch try/install-me -no-verify >"$TMP/out-update.txt" 2>&1; then
	check "mole update -branch exits 0" true
else
	check "mole update -branch exits 0" false
fi
check "it is now the branch build" version_has "$TMP/self/mole" "(try/install-me@$WANT)"

echo "== mole update -branch with a branch that does not exist"
cp "$TMP/self/mole" "$TMP/self/mole.before"
if env "${update_env[@]}" "$TMP/self/mole" update -branch no/such-branch -no-verify >"$TMP/out-update-missing.txt" 2>&1; then
	check "it fails" false
else
	check "it fails" true
fi
check "and does not claim to have updated" bash -c '! grep -qF "updated successfully" "$1"' _ "$TMP/out-update-missing.txt"
check "the running binary was left alone" cmp -s "$TMP/self/mole" "$TMP/self/mole.before"
check "-branch with -version is refused" bash -c '! "$1" update -branch a -version v0.1.1 -dry-run' _ "$TMP/self/mole"
check "a dry run shows the branch and installer" bash -c '"$1" update -branch feat/x -dry-run | grep -qF "refs/heads/feat/x/scripts/install.sh"' _ "$TMP/self/mole"

echo "== Go too old or missing: the installer fetches the one the source needs"
WANT_GO="$(awk '$1 == "go" { print $2; exit }' "$ROOT/go.mod")"
REAL_GO="$(command -v go)"
case "$(uname -s)" in Linux) GOOS_=linux ;; Darwin) GOOS_=darwin ;; *) GOOS_=freebsd ;; esac
case "$(uname -m)" in x86_64|amd64) GOARCH_=amd64 ;; aarch64|arm64) GOARCH_=arm64 ;; *) GOARCH_=$(uname -m) ;; esac
# A stand-in Go "mirror": an archive whose go/bin/go runs the real one (letting it
# pick the toolchain go.mod names, as the real downloaded Go would be), with its digest.
mkdir -p "$TMP/gomirror" "$TMP/gosrc/go/bin"
printf '#!/bin/sh\necho "$0" >> "%s/fetched-go-used"\nGOTOOLCHAIN=auto exec "%s" "$@"\n' "$TMP" "$REAL_GO" >"$TMP/gosrc/go/bin/go"
chmod 755 "$TMP/gosrc/go/bin/go"
GOFILE="go$WANT_GO.$GOOS_-$GOARCH_.tar.gz"
tar -C "$TMP/gosrc" -czf "$TMP/gomirror/$GOFILE" go
if command -v sha256sum >/dev/null 2>&1; then sha256sum "$TMP/gomirror/$GOFILE" | awk '{print $1}' >"$TMP/gomirror/$GOFILE.sha256"; else shasum -a 256 "$TMP/gomirror/$GOFILE" | awk '{print $1}' >"$TMP/gomirror/$GOFILE.sha256"; fi
# An outdated Go first on PATH.
mkdir -p "$TMP/oldgo"
printf '#!/bin/sh\n[ "$1 $2" = "env GOVERSION" ] && { echo go1.20.0; exit 0; }\nexit 1\n' >"$TMP/oldgo/go"
chmod 755 "$TMP/oldgo/go"
godl() { # godl <dest> [extra env...]
	local dest="$1"
	shift
	env PATH="$TMP/oldgo:$PATH" XDG_CACHE_HOME="$TMP/cache" MOLE_GO_URL="file://$TMP/gomirror" MOLE_REPO="$TMP/origin.git" \
		MOLE_VERSION=try/install-me INSTALL_DIR="$dest" NO_COLOR=1 "$@" bash "$ROOT/scripts/install.sh" --no-verify
}
if godl "$TMP/bin-godl/mole" >"$TMP/out-godl.txt" 2>&1; then
	check "an old Go is replaced by the downloaded one" true
else
	check "an old Go is replaced by the downloaded one" false
fi
check "the build used the downloaded Go" test -s "$TMP/fetched-go-used"
check "mole built with it" version_has "$TMP/bin-godl/mole" "(try/install-me@$WANT)"
check "it was cached for next time" test -x "$TMP/cache/mole/go/$WANT_GO/go/bin/go"
rm -f "$TMP/fetched-go-used"
godl "$TMP/bin-godl2/mole" >"$TMP/out-godl2.txt" 2>&1 || true
check "the cached Go is reused, not downloaded again" bash -c '! grep -q downloading "$1"' _ "$TMP/out-godl2.txt"
echo "== a tampered Go archive is refused"
echo 0000000000000000000000000000000000000000000000000000000000000000 >"$TMP/gomirror/$GOFILE.sha256"
rm -rf "$TMP/cache"
if godl "$TMP/bin-godl3/mole" >"$TMP/out-godl3.txt" 2>&1; then
	check "a checksum mismatch fails the install" false
else
	check "a checksum mismatch fails the install" true
fi
check "nothing was installed" test ! -e "$TMP/bin-godl3/mole"
check "nothing was cached" test ! -e "$TMP/cache/mole/go/$WANT_GO"
echo "== MOLE_NO_GO_DOWNLOAD refuses to download"
if godl "$TMP/bin-godl4/mole" MOLE_NO_GO_DOWNLOAD=1 >"$TMP/out-godl4.txt" 2>&1; then
	check "it fails instead of downloading" false
else
	check "it fails instead of downloading" true
fi
check "and says what is needed" grep -q "is required" "$TMP/out-godl4.txt"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
if [ "$fail" -ne 0 ]; then
	echo "--- installer output of the runs above:" >&2
	tail -n 12 "$TMP"/out-*.txt >&2 || true
	exit 1
fi
