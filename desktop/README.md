# Mole desktop

The desktop app controls the Mole SSH forwarder. Run it as your normal user,
never with `sudo`. The Setup tab installs the bundled CLI into a per-user
location; it does not require an administrator.

## Linux installation and window integration

The Debian, RPM and Arch packages install the GUI at `/usr/lib/mole/mole`,
outside `PATH`, because the CLI owns the `mole` command. Their desktop entry is
`dev.luqueee.mole.desktop` and executes that absolute path. The GTK application
ID matches the entry so Wayland desktops can associate the window, taskbar and
icon correctly. AppImage uses its own entry with a bundle-relative executable;
it must not reuse the system-package entry.

The Linux tasks use `MOLE_DESKTOP_ID` for the launcher identity, separate from
Android's `APP_ID`. CI invokes the Debian and AppImage packaging tasks in
separate Wails commands so both artifacts are built.

System packages require the package manager's usual installation permission.
The AppImage can be run from a user-owned directory without installing a system
package. Linux runtime requirements are GTK 4 and WebKitGTK 6.0 (Debian 13+ or
Ubuntu 24.04+).

Windows, macOS and Linux use native window decorations. On Linux their appearance
is controlled by the desktop environment and window manager, not a hard-coded
distribution theme. The application's content retains the Mole theme.

## Optional system DNS

Ordinary port forwarding and named HTTP routes under a `.localhost` domain work
without administrator access. In Proxy, set a domain such as `dev.localhost`,
add a route such as `web -> 3000`, and open `http://web.dev.localhost:7080` (or
your configured `route_port`). Localhost-only configurations start the route
proxy without binding the system DNS stub. The app reports DNS as **NOT REQUIRED**.

Custom domains such as `dev.lan` require system DNS integration for automatic
name resolution. The Proxy tab offers this separately, using Polkit on Linux
(`pkexec`) or the system authorization dialog on macOS. Only the DNS command is
elevated; the GUI, CLI installation and daemon remain user processes. Cancelling
leaves the app usable. After installation, an already-running daemon is restarted
with the explicit user configuration; a stopped daemon stays stopped. Failed
installation or reload is reported rather than shown as success.

Without system DNS, the app does not offer broken OPEN links for custom domains.
Use `.localhost` routes or the ordinary forwarded-port URLs instead. Remote
wildcard split DNS (for example every name under `kena.lan`) still needs the
system integration; `.localhost` aliases do not change the remote DNS.

## Development and packaging

Use Go 1.26.4+, Node.js and Wails CLI `v3.0.0-beta.34`. Native Linux builds also
need `libgtk-4-dev` and `libwebkitgtk-6.0-dev`.

From `desktop/`:

```sh
wails3 task bundle:cli
wails3 build
wails3 task linux:create:deb
wails3 task linux:create:appimage
```

For development, run `wails3 dev`. The existing `build:server` task exposes the
same backend/frontend over HTTP for browser-based inspection.

## Branding assets

The canonical icon is `../landing/public/mole-mark.png`, shared with the landing
page. `build/appicon-128.png` is its exact package-size copy. The larger app icon,
tray images and Icon Composer source are checked-in derivatives, so normal
builds do not require ImageMagick.

When intentionally changing the mark, regenerate the derivatives from `desktop/`
with ImageMagick available, then regenerate the platform icons:

```sh
wails3 task common:generate:brand-assets
wails3 task common:generate:icons
```

Wails generates Windows ICO and macOS ICNS on supported build hosts. The compiled
macOS `Assets.car` is a build artifact, generated from the same Icon Composer
source on macOS 26+; it is not checked in, so older builds fall back to the
branded ICNS rather than shipping an obsolete asset catalog.
