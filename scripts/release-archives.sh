#!/usr/bin/env bash
# scripts/release-archives.sh — build the release archives of mole.
#
#   ./scripts/release-archives.sh 0.4.0 [outdir]
#
# Produces, in outdir (default: dist/release-<version>), one archive per
# platform holding the binary and the licence, plus SHA256SUMS:
#
#   mole_<version>_<os>_<arch>.tar.gz   (linux, darwin, freebsd; amd64, arm64)
#   mole_<version>_windows_<arch>.zip
#
# That is the layout scripts/install.ps1 downloads (it looks the zip up by name
# and checks the digest GitHub shows for it). The binaries are static
# (CGO_ENABLED=0), trimmed, and stamped with the tag and commit, which
# `mole version` prints.
#
# It refuses to run unless the version in cmd/mole/main.go is the one asked for
# and the working tree has no uncommitted changes, so an archive always
# corresponds to a commit. Publishing is a separate, deliberate step:
#
#   git tag v0.4.0 && git push origin v0.4.0
#   gh release create v0.4.0 dist/release-0.4.0/* --title "mole v0.4.0" --notes-file NOTES.md

set -euo pipefail

VERSION="${1:-}"
if [ -z "$VERSION" ] || [ "${VERSION#v}" != "$VERSION" ]; then
	echo "usage: $0 VERSION [outdir]   (VERSION without the leading v, e.g. 0.4.0)" >&2
	exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"
OUT="${2:-$ROOT/dist/release-$VERSION}"
cd "$ROOT"

if ! grep -qF "const version = \"$VERSION\"" cmd/mole/main.go; then
	echo "error: cmd/mole/main.go does not say version $VERSION; bump it first" >&2
	exit 1
fi
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	echo "error: the working tree has uncommitted changes; commit them so the archives match a commit" >&2
	exit 1
fi
COMMIT="$(git rev-parse --short=7 HEAD)"
LDFLAGS="-s -w -X main.buildRef=v$VERSION -X main.buildCommit=$COMMIT"

rm -rf "$OUT"
mkdir -p "$OUT"
for target in darwin/amd64 darwin/arm64 freebsd/amd64 freebsd/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
	os="${target%/*}"
	arch="${target#*/}"
	stage="$(mktemp -d "$OUT/stage.XXXXXX")"
	exe=mole
	[ "$os" = windows ] && exe=mole.exe
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" -o "$stage/$exe" ./cmd/mole
	cp LICENSE "$stage/LICENSE"
	if [ "$os" = windows ]; then
		(cd "$stage" && zip -q -X "$OUT/mole_${VERSION}_${os}_${arch}.zip" "$exe" LICENSE)
	else
		chmod 755 "$stage/$exe"
		tar -C "$stage" -czf "$OUT/mole_${VERSION}_${os}_${arch}.tar.gz" "$exe" LICENSE
	fi
	rm -rf "$stage"
	echo "built $os/$arch" >&2
done

cd "$OUT"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum mole_* >SHA256SUMS
else
	shasum -a 256 mole_* >SHA256SUMS
fi
cat SHA256SUMS
