# scripts/

Build, install, uninstall. **Configuration lives in the binary** (`mole init`),
not in these scripts — this directory is intentionally small and dumb on purpose.

| Script             | OS                        | What it does                                                  |
| ------------------ | ------------------------- | ------------------------------------------------------------- |
| `install.sh`       | Linux, macOS, FreeBSD     | Build or clone, install, verify, print `mole init` hint.      |
| `install.ps1`      | Windows                   | Same as `install.sh`, PowerShell-flavoured.                   |
| `uninstall.sh`     | Linux, macOS, FreeBSD     | Remove the binary; leave the config (`mole.yaml`) alone.     |
| `uninstall.ps1`    | Windows                   | Same as `uninstall.sh`, PowerShell-flavoured.                 |

## Why no `init`/`init.sh` here

There used to be a `scripts/init` that asked for the remote, ports, and where
to write the config. We removed it because:

- It duplicated the logic that's already in `mole init` (a subcommand of the
  binary).
- It couldn't be the single source of truth: the Windows installer had its own
  prompt set, the Bash one had a third. They drifted.
- The binary is the only thing that knows what the config schema looks like,
  so the prompts belong next to the loader.

Now `install.sh` and `install.ps1` only build, copy, verify, and tell the user
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

| Variable        | Used by                 | Purpose                                           |
| --------------- | ----------------------- | ------------------------------------------------- |
| `MOLE_VERSION`  | `install.sh`/`.ps1`     | Git ref to checkout when cloning (default: main). |
| `MOLE_SRC`      | `install.sh`/`.ps1`     | Path to a local clone to build from, skip clone.  |
| `GO`            | `install.sh`            | Path to a specific `go` binary.                   |
| `INSTALL_DIR`   | both installers         | Absolute path of the installed binary; overrides `--prefix`. |
