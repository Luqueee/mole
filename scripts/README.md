# scripts/

Build, install, uninstall. **Configuration lives in the binary** (`mole init`),
not in these scripts — this directory is intentionally small and dumb on purpose.

| Script             | OS                        | What it does                                                  |
| ------------------ | ------------------------- | ------------------------------------------------------------- |
| `install.sh`       | Linux, macOS, FreeBSD     | Installs a verified release archive, or builds from source.   |
| `install.ps1`      | Windows                   | Builds from a clone or installs a verified release archive.  |
| `release-archives.sh` | any (maintainers)      | Build the per-platform release archives and `SHA256SUMS`.     |
| `uninstall.sh`     | Linux, macOS, FreeBSD     | Remove the binary and the login service (`mole service`); leave the config (`mole.yaml`) alone. |
| `uninstall.ps1`    | Windows                   | Same as `uninstall.sh`, PowerShell-flavoured (Windows has no `mole service`). |
| `test-install-branch.sh` | Linux, macOS (CI)   | Offline checks of source builds (branches, tags, Go download), then runs `test-install-release.sh`. |
| `test-install-release.sh` | Linux, macOS (CI)  | Offline checks of release installs and `mole update` against a fake release. |

## What gets installed

Both installers install the **latest CLI release** by default: the newest
published (not draft, not prerelease) release tagged `vX.Y.Z`. The desktop
app's releases (`desktop-v*`) live in the same repository and are skipped. The
archive for the platform (`mole_<version>_<os>_<arch>.tar.gz`, or `.zip` on
Windows, as `release-archives.sh` builds them) is checked before it is
installed: on Unix against the release's `SHA256SUMS`, failing when the
checksum does not match or the file does not list the archive; on Windows
against the SHA-256 digest GitHub reports for the asset. A release install on
Unix needs curl or wget, tar, and sha256sum or shasum (or FreeBSD's sha256,
or openssl) — no git, no Go. When the GitHub CLI is installed and signed in,
`install.sh` also runs `gh attestation verify` on the archive (see
`MOLE_ATTESTATION`).

`install.sh` builds from source instead (git and Go 1.26.10+; Go is downloaded
into `~/.cache/mole/go` when missing or too old) when it runs inside a mole
clone, for `MOLE_SRC`, for `MOLE_REPO`, for a `MOLE_VERSION` that is not a
`vX.Y.Z` release tag (a branch, say), and when the release has no archive for
the platform (another OS or CPU, or a tag without a release): it says why, and
builds that release's tag. Builds are stamped with the ref and commit, which
`mole version` prints.

`mole update` re-runs the installer with `INSTALL_DIR` set to the running
binary and `MOLE_RELEASE_ONLY=1`, so it installs the latest release (or
`-version`'s) even when run inside a clone; `-branch` builds from source.

## Why no `init`/`init.sh` here

There used to be a `scripts/init` that asked for the remote, ports, and where
to write the config. We removed it because:

- It duplicated the logic that's already in `mole init` (a subcommand of the
  binary).
- It couldn't be the single source of truth: the Windows installer had its own
  prompt set, the Bash one had a third. They drifted.
- The binary is the only thing that knows what the config schema looks like,
  so the prompts belong next to the loader.

Now `install.sh` and `install.ps1` only acquire, copy, verify, and tell the user
to run `mole init`. The interactive prompts are **identical on every OS**
because they live in one place: `internal/config/init.go`.

## Usage

```sh
# from a clone
./scripts/install.sh

# one-liner (Unix)
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh | sh

# custom prefix
./scripts/install.sh --prefix /opt

# also run mole init right away (interactive)
./scripts/install.sh --init

# a given release, or a branch built from source
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh | MOLE_VERSION=v0.6.1 sh
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh | MOLE_VERSION=feat/x sh

# fully automatic install (binary + non-interactive mole init).
# Any MOLE_* env var listed under `mole init -h` is honoured.
curl -fsSL https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.sh \
  | MOLE_REMOTE=dev@workstation sh -s -- --init
```

```powershell
# from a clone
.\scripts\install.ps1

# one-liner (Windows)
irm https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.ps1 | iex

# also run mole init right away
.\scripts\install.ps1 -Init
```

## Environment variables

| Variable            | Used by             | Purpose                                           |
| ------------------- | ------------------- | ------------------------------------------------- |
| `MOLE_VERSION`      | `install.sh`/`.ps1` | Release tag to install (default: the latest release). On Unix, any other Git ref (a branch, `main`) is built from source; a ref that does not exist is an error. |
| `MOLE_BRANCH`       | `install.ps1`       | Branch (or tag) to download and build with Go.    |
| `MOLE_SRC`          | `install.sh`/`.ps1` | Path to a local clone to build from, skip clone.  |
| `MOLE_REPO`         | `install.sh`        | Git remote to build from (a fork, or a local repository); always a source build. |
| `MOLE_RELEASE_ONLY` | `install.sh`/`.ps1` | Ignore a mole clone in the working directory (`mole update` sets it). |
| `GITHUB_TOKEN`      | `install.sh`/`.ps1` | Sent to `api.github.com` (only) when looking up releases, for its rate limit. |
| `MOLE_ATTESTATION`  | `install.sh`        | `auto` (default: check with `gh attestation verify` when gh is signed in; a failure is a warning), `require`, or `off`. |
| `MOLE_API_URL`      | `install.sh`        | GitHub API base of the repository (default `https://api.github.com/repos/Luqueee/mole`); tests point it at a `file://` fixture. |
| `MOLE_RELEASE_URL`  | `install.sh`        | Base the archives are downloaded from (default `https://github.com/Luqueee/mole/releases/download`). |
| `GO`                | `install.sh`        | Path to a specific `go` binary.                   |
| `MOLE_NO_GO_DOWNLOAD` | `install.sh`      | Fail instead of downloading Go for a source build. |
| `INSTALL_DIR`       | both installers     | Absolute path of the installed binary; overrides `--prefix`. |
