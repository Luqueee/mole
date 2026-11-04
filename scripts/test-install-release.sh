#!/usr/bin/env bash
# scripts/test-install-release.sh — check that install.sh, and `mole update`
# through it, install a release: the archive for this platform, checked against
# the release's SHA256SUMS, with neither git nor Go. Also that an archive whose
# checksum does not match (or that SHA256SUMS does not list, or a release with
# no SHA256SUMS) is refused and nothing is installed; that the desktop app's
# releases (desktop-v*), drafts and prereleases are never "the latest"; and that
# a release with no archive for the platform is built from its tag instead.
#
# It needs no network: MOLE_API_URL and MOLE_RELEASE_URL point the installer at
# a fake release in a temporary directory, through file://, laid out the way
# GitHub serves it, with archives made the way scripts/release-archives.sh makes
# them. scripts/test-install-branch.sh runs it too.
#
#   ./scripts/test-install-release.sh
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

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

case "$(uname -s)" in
Linux) OS=linux ;;
Darwin) OS=darwin ;;
FreeBSD) OS=freebsd ;;
*)
	echo "skipped: mole publishes no release archives for $(uname -s)"
	exit 0
	;;
esac

# ---------------------------------------------------------------------------
# A fake GitHub: an API answer listing the releases, and their downloads.
# ---------------------------------------------------------------------------

# Two builds of this tree, stamped as two releases. Every other archive holds
# the older one, so installing a release that should not have been picked
# shows in `mole version`.
for v in 9.9.9 9.9.8; do
	mkdir -p "$TMP/build-$v"
	(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X main.buildRef=v$v -X main.buildCommit=fake${v//./}" \
		-o "$TMP/build-$v/mole" ./cmd/mole)
	cp "$ROOT/LICENSE" "$TMP/build-$v/LICENSE"
	chmod 755 "$TMP/build-$v/mole"
done

DL="$TMP/download"
# release <tag> <build>: the archives of a release, for this OS (amd64 and
# arm64 both hold this machine's binary), and their SHA256SUMS.
release() {
	local tag="$1" build="$2" ver="${1#v}" arch
	mkdir -p "$DL/$tag"
	for arch in amd64 arm64; do
		tar -C "$TMP/build-$build" -czf "$DL/$tag/mole_${ver}_${OS}_${arch}.tar.gz" mole LICENSE
	done
	(cd "$DL/$tag" && sha256 mole_* >SHA256SUMS)
}
release v9.9.9 9.9.9
release v9.9.8 9.9.8
release v10.0.0 9.9.8 # a prerelease
release v10.1.0 9.9.8 # a draft
# An archive that is not the one SHA256SUMS describes.
release v9.9.7 9.9.8
echo tampered >>"$DL/v9.9.7/mole_9.9.7_${OS}_amd64.tar.gz"
echo tampered >>"$DL/v9.9.7/mole_9.9.7_${OS}_arm64.tar.gz"
# A SHA256SUMS that does not list this platform's archives.
release v9.9.6 9.9.8
grep -vF "_${OS}_" "$DL/v9.9.6/SHA256SUMS" >"$DL/v9.9.6/SHA256SUMS.new" || true
mv "$DL/v9.9.6/SHA256SUMS.new" "$DL/v9.9.6/SHA256SUMS"
# No SHA256SUMS at all.
release v9.9.5 9.9.8
rm "$DL/v9.9.5/SHA256SUMS"
# A release with no archive for this OS.
mkdir -p "$DL/v9.9.4"
: >"$DL/v9.9.4/SHA256SUMS"

# rel <tag> <draft> <prerelease> [notes]: a release as the API lists it, with
# the nested objects (author, assets, uploader) a real answer has.
rel() {
	cat <<EOF
  {
    "url": "https://api.github.com/repos/Luqueee/mole/releases/1",
    "id": 1,
    "author": { "login": "luqueee", "id": 1, "type": "User", "site_admin": false },
    "tag_name": "$1",
    "target_commitish": "main",
    "name": "mole $1",
    "draft": $2,
    "immutable": false,
    "prerelease": $3,
    "created_at": "2026-10-24T17:14:14Z",
    "assets": [
      { "name": "SHA256SUMS", "label": "", "state": "uploaded",
        "uploader": { "login": "github-actions[bot]", "type": "Bot" },
        "digest": "sha256:0000", "browser_download_url": "https://github.com/Luqueee/mole/releases/download/$1/SHA256SUMS" }
    ],
    "body": "${4:-}"
  }
EOF
}
mkdir -p "$TMP/api" "$TMP/api-none"
{
	echo "["
	rel desktop-v99.0.0 false false 'The desktop app.'
	echo ","
	rel v10.1.0 true false
	echo ","
	rel v10.0.0 false true
	echo ","
	# Notes that quote the fields must not pass for them.
	rel v9.9.9 false false 'Not \"tag_name\": \"v99.0.0\", \"draft\": false, \"prerelease\": false.'
	echo ","
	rel v1.2 false false
	echo ","
	# The same fields, in another order and on one line.
	echo '  {"prerelease":false,"name":"mole v9.9.8","draft":false,"assets":[],"tag_name":"v9.9.8"}'
	echo "]"
} >"$TMP/api/releases"
{
	echo "["
	rel desktop-v0.8.4 false false
	echo ","
	rel v10.0.0 false true
	echo "]"
} >"$TMP/api-none/releases"

# The installer as `mole update` and `curl | sh` get it: a file outside any
# mole clone.
mkdir -p "$TMP/installer" "$TMP/raw/main/scripts"
cp "$ROOT/scripts/install.sh" "$TMP/installer/install.sh"
cp "$ROOT/scripts/install.sh" "$TMP/raw/main/scripts/install.sh"

# A git and a go that only say they were run: a release install needs neither.
mkdir -p "$TMP/nobuild"
for tool in git go; do
	printf '#!/bin/sh\necho "%s $*" >>"%s/build-tools-used"\nexit 1\n' "$tool" "$TMP" >"$TMP/nobuild/$tool"
	chmod 755 "$TMP/nobuild/$tool"
done
no_build_tools() { test ! -e "$TMP/build-tools-used"; }

# A gh whose answers the test picks.
mkdir -p "$TMP/gh"
cat >"$TMP/gh/gh" <<EOF
#!/bin/sh
echo "gh \$*" >>"$TMP/gh-used"
case "\$1" in
auth) [ "\${FAKE_GH_AUTH:-yes}" = yes ] ;;
attestation) [ "\${FAKE_GH_VERIFY:-yes}" = yes ] ;;
*) exit 1 ;;
esac
EOF
chmod 755 "$TMP/gh/gh"

mkdir "$TMP/cwd"
cd "$TMP/cwd"
fake_env=(MOLE_API_URL="file://$TMP/api" MOLE_RELEASE_URL="file://$TMP/download" NO_COLOR=1)
install_release() { # install_release <dest> [env...] -- [installer args...]
	local dest="$1"
	shift
	env -u GO -u GITHUB_TOKEN -u MOLE_REPO -u MOLE_SRC -u MOLE_VERSION \
		PATH="$TMP/nobuild:$PATH" MOLE_ATTESTATION=off "${fake_env[@]}" INSTALL_DIR="$dest" "$@" \
		bash "$TMP/installer/install.sh" --no-verify
}

echo "== no MOLE_VERSION: the latest release"
rm -f "$TMP/build-tools-used"
if env -u GO -u GITHUB_TOKEN -u MOLE_REPO -u MOLE_SRC -u MOLE_VERSION PATH="$TMP/nobuild:$PATH" MOLE_ATTESTATION=off \
	"${fake_env[@]}" INSTALL_DIR="$TMP/bin-latest/mole" bash "$TMP/installer/install.sh" >"$TMP/out-latest.txt" 2>&1; then
	check "the installer exits 0" true
else
	check "the installer exits 0" false
fi
check "it is the newest published CLI release, not the desktop app, a draft or a prerelease" \
	version_has "$TMP/bin-latest/mole" "(v9.9.9@fake999)"
check "it says which release it found" grep -qF "latest release: v9.9.9" "$TMP/out-latest.txt"
check "it checked the archive's SHA-256" grep -qF "verified the SHA-256 of mole_9.9.9_${OS}_" "$TMP/out-latest.txt"
check "the post-install check ran it" grep -qF "(v9.9.9@fake999)" "$TMP/out-latest.txt"
check "neither git nor go was needed" no_build_tools

echo "== piped into a POSIX sh (dash on Debian and Ubuntu), as in curl ... | sh"
if env -u GO -u GITHUB_TOKEN -u MOLE_REPO -u MOLE_SRC -u MOLE_VERSION PATH="$TMP/nobuild:$PATH" MOLE_ATTESTATION=off \
	"${fake_env[@]}" INSTALL_DIR="$TMP/bin-sh/mole" sh -s -- --no-verify <"$TMP/installer/install.sh" >"$TMP/out-sh.txt" 2>&1; then
	check "sh installs and exits 0" true
else
	check "sh installs and exits 0" false
fi
check "and it is the latest release" version_has "$TMP/bin-sh/mole" "(v9.9.9@fake999)"
check "still without git or go" no_build_tools

echo "== inside a mole clone with MOLE_RELEASE_ONLY (what mole update sets)"
(cd "$ROOT" && install_release "$TMP/bin-clone/mole" MOLE_RELEASE_ONLY=1) >"$TMP/out-clone.txt" 2>&1 || true
check "the release is installed, not the clone" version_has "$TMP/bin-clone/mole" "(v9.9.9@fake999)"

echo "== MOLE_VERSION=v9.9.8: that release, without asking the API"
install_release "$TMP/bin-pin/mole" MOLE_VERSION=v9.9.8 MOLE_API_URL="file://$TMP/no-api" >"$TMP/out-pin.txt" 2>&1 || true
check "the pinned release is installed" version_has "$TMP/bin-pin/mole" "(v9.9.8@fake998)"
check "neither git nor go was needed" no_build_tools

echo "== an archive that does not match SHA256SUMS"
mkdir -p "$TMP/bin-bad"
cp "$TMP/bin-pin/mole" "$TMP/bin-bad/mole"
if install_release "$TMP/bin-bad/mole" MOLE_VERSION=v9.9.7 >"$TMP/out-mismatch.txt" 2>&1; then
	check "the install fails" false
else
	check "the install fails" true
fi
check "the binary already there was left alone" cmp -s "$TMP/bin-bad/mole" "$TMP/bin-pin/mole"
check "the error says the checksum does not match" grep -qF "does not match its checksum" "$TMP/out-mismatch.txt"
check "and it did not fall back to a build" no_build_tools

echo "== a SHA256SUMS without the archive, and a release without SHA256SUMS"
if install_release "$TMP/bin-nosum/mole" MOLE_VERSION=v9.9.6 >"$TMP/out-nosum.txt" 2>&1; then
	check "an unlisted archive fails the install" false
else
	check "an unlisted archive fails the install" true
fi
check "nothing was installed" test ! -e "$TMP/bin-nosum/mole"
check "the error says SHA256SUMS has no checksum for it" grep -qF "has no checksum for mole_9.9.6_${OS}_" "$TMP/out-nosum.txt"
if install_release "$TMP/bin-nosums/mole" MOLE_VERSION=v9.9.5 >"$TMP/out-nosums.txt" 2>&1; then
	check "a release without SHA256SUMS fails the install" false
else
	check "a release without SHA256SUMS fails the install" true
fi
check "nothing was installed" test ! -e "$TMP/bin-nosums/mole"
check "the error says so" grep -qF "could not download the SHA256SUMS of v9.9.5" "$TMP/out-nosums.txt"

echo "== no published CLI release, or no answer"
if install_release "$TMP/bin-none/mole" MOLE_API_URL="file://$TMP/api-none" >"$TMP/out-none.txt" 2>&1; then
	check "only desktop releases and prereleases: the install fails" false
else
	check "only desktop releases and prereleases: the install fails" true
fi
check "the error says there is no release" grep -qF "found no published mole release" "$TMP/out-none.txt"
if install_release "$TMP/bin-noapi/mole" MOLE_API_URL="file://$TMP/no-api" >"$TMP/out-noapi.txt" 2>&1; then
	check "an API that does not answer fails the install" false
else
	check "an API that does not answer fails the install" true
fi
check "the error says the releases could not be listed" grep -qF "could not list the releases" "$TMP/out-noapi.txt"
check "nothing was installed" test ! -e "$TMP/bin-none/mole" -a ! -e "$TMP/bin-noapi/mole"

echo "== a release without an archive for this platform is built from its tag"
if install_release "$TMP/bin-noarchive/mole" MOLE_VERSION=v9.9.4 >"$TMP/out-noarchive.txt" 2>&1; then
	check "(the stand-in git cannot clone)" false
else
	check "(the stand-in git cannot clone)" true
fi
check "it says why it builds" grep -qF "release v9.9.4 has no mole_9.9.4_${OS}_" "$TMP/out-noarchive.txt"
check "and clones that tag" grep -qF "git clone --depth 1 --branch v9.9.4 https://github.com/Luqueee/mole.git" "$TMP/build-tools-used"
rm -f "$TMP/build-tools-used"

echo "== a platform releases have no archive for: the latest release's tag, built"
# git is real here, with the official remote redirected to a copy of this repository.
git clone -q --bare "$ROOT" "$TMP/origin.git"
git -C "$TMP/origin.git" tag v9.9.9 HEAD
WANT="$(git -C "$TMP/origin.git" rev-parse --short=7 v9.9.9)"
mkdir -p "$TMP/riscv"
printf '#!/bin/sh\n[ "$1" = -m ] && { echo riscv64; exit 0; }\nexec "%s" "$@"\n' "$(command -v uname)" >"$TMP/riscv/uname"
chmod 755 "$TMP/riscv/uname"
if env -u GO -u GITHUB_TOKEN -u MOLE_REPO -u MOLE_SRC -u MOLE_VERSION PATH="$TMP/riscv:$PATH" MOLE_ATTESTATION=off "${fake_env[@]}" \
	GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.file://$TMP/origin.git.insteadOf" GIT_CONFIG_VALUE_0="https://github.com/Luqueee/mole.git" \
	INSTALL_DIR="$TMP/bin-riscv/mole" bash "$TMP/installer/install.sh" --no-verify >"$TMP/out-riscv.txt" 2>&1; then
	check "the installer exits 0" true
else
	check "the installer exits 0" false
fi
check "it says why it builds" grep -qF "mole releases have no archive for $(uname -s)/riscv64" "$TMP/out-riscv.txt"
check "the build is of the release's tag" version_has "$TMP/bin-riscv/mole" "(v9.9.9@$WANT)"

echo "== build provenance (MOLE_ATTESTATION, with gh)"
gh_install() { # gh_install <dest> <mode> [env...]
	local dest="$1" mode="$2"
	shift 2
	env -u GO -u GITHUB_TOKEN -u MOLE_REPO -u MOLE_SRC -u MOLE_VERSION PATH="$TMP/gh:$TMP/nobuild:$PATH" \
		"${fake_env[@]}" MOLE_ATTESTATION="$mode" INSTALL_DIR="$dest" "$@" bash "$TMP/installer/install.sh" --no-verify
}
rm -f "$TMP/gh-used"
gh_install "$TMP/bin-att/mole" require >"$TMP/out-att.txt" 2>&1 || true
check "require: a verified archive installs" version_has "$TMP/bin-att/mole" "(v9.9.9@fake999)"
check "gh checked it against Luqueee/mole" grep -qE "gh attestation verify .*/mole_9.9.9_${OS}_[a-z0-9]+\.tar\.gz --repo Luqueee/mole" "$TMP/gh-used"
if gh_install "$TMP/bin-att-bad/mole" require FAKE_GH_VERIFY=no >"$TMP/out-att-bad.txt" 2>&1; then
	check "require: an archive gh cannot verify fails the install" false
else
	check "require: an archive gh cannot verify fails the install" true
fi
check "nothing was installed" test ! -e "$TMP/bin-att-bad/mole"
gh_install "$TMP/bin-att-auto/mole" auto FAKE_GH_VERIFY=no >"$TMP/out-att-auto.txt" 2>&1 || true
check "auto: an archive gh cannot verify still installs" version_has "$TMP/bin-att-auto/mole" "(v9.9.9@fake999)"
check "with a warning" grep -qF "could not verify the build provenance" "$TMP/out-att-auto.txt"
rm -f "$TMP/gh-used"
gh_install "$TMP/bin-att-noauth/mole" auto FAKE_GH_AUTH=no >"$TMP/out-att-noauth.txt" 2>&1 || true
check "auto: a gh that is not signed in is not asked" bash -c '! grep -q attestation "$1"' _ "$TMP/gh-used"
check "and the release installs" version_has "$TMP/bin-att-noauth/mole" "(v9.9.9@fake999)"

echo "== mole update installs the latest release over the running binary"
mkdir -p "$TMP/self"
(cd "$ROOT" && go build -o "$TMP/self/mole" ./cmd/mole)
update_env=(MOLE_RAW_URL="file://$TMP/raw" "${fake_env[@]}" MOLE_ATTESTATION=off PATH="$TMP/nobuild:$PATH")
check "a dry run says it installs the latest release" \
	bash -c '"$1" update -dry-run | grep -qF "latest release"' _ "$TMP/self/mole"
# From inside a mole clone: it must still be the release, not a build of the clone.
if (cd "$ROOT" && env -u GO -u MOLE_REPO -u MOLE_SRC -u MOLE_VERSION "${update_env[@]}" "$TMP/self/mole" update -no-verify) >"$TMP/out-update.txt" 2>&1; then
	check "mole update exits 0" true
else
	check "mole update exits 0" false
fi
check "it is now the latest release" version_has "$TMP/self/mole" "(v9.9.9@fake999)"
check "neither git nor go was needed" no_build_tools
env -u GO -u MOLE_REPO -u MOLE_SRC -u MOLE_VERSION "${update_env[@]}" "$TMP/self/mole" update -version v9.9.8 -no-verify >"$TMP/out-update-pin.txt" 2>&1 || true
check "mole update -version v9.9.8 installs that release" version_has "$TMP/self/mole" "(v9.9.8@fake998)"
cp "$TMP/self/mole" "$TMP/self/mole.before"
if env -u GO -u MOLE_REPO -u MOLE_SRC -u MOLE_VERSION "${update_env[@]}" "$TMP/self/mole" update -version v9.9.7 -no-verify >"$TMP/out-update-bad.txt" 2>&1; then
	check "mole update with a tampered archive fails" false
else
	check "mole update with a tampered archive fails" true
fi
check "and does not claim to have updated" bash -c '! grep -qF "updated successfully" "$1"' _ "$TMP/out-update-bad.txt"
check "the running binary was left alone" cmp -s "$TMP/self/mole" "$TMP/self/mole.before"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
if [ "$fail" -ne 0 ]; then
	echo "--- installer output of the runs above:" >&2
	tail -n 12 "$TMP"/out-*.txt >&2 || true
	exit 1
fi
