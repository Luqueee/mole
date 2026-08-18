# mole — landing page

Static landing page for [mole](https://github.com/Luqueee/mole), built with
Astro + Tailwind v4 — no UI framework, no client-side JS runtime. Output is a
fully static site in `dist/`.

## Develop

```bash
pnpm install     # npm also works; both lockfiles are committed
pnpm dev         # http://localhost:4321
```

## Build

```bash
pnpm build       # → dist/
pnpm preview     # serve dist/ locally
pnpm typecheck   # astro check
pnpm lint        # eslint
pnpm format      # prettier
```

## Deploy

The site is self-hosted on the `websites` box (LXC 300 on px2), the same one
that serves the other sites, and reaches the internet through the Cloudflare
tunnel already running there — `mole.luqueee.dev` is an ingress rule pointing at
`http://127.0.0.1:6768`, so this process never sees TLS or the public hostname.

There is no adaptor: the build is static, and pm2's own static server serves
`dist/` through `ecosystem.config.cjs`. It runs under the `web` user, whose pm2
daemon has a systemd unit and therefore survives a reboot.

The box declares itself in `landing/.env`, which is not committed:

```ini
MOLE_LANDING_URL=https://mole.luqueee.dev
MOLE_UMAMI_SCRIPT_URL=https://analytics.luqueee.dev/script.js
MOLE_UMAMI_WEBSITE_ID=<the id the umami instance minted for this site>
```

`astro.config.mjs` reads that file with `process.loadEnvFile()`, so a build on
the box needs nothing exported. The analytics tracker needs **both** umami
variables or it is not emitted at all, which is what keeps `astro dev` and a
local build out of the production dataset.

Redeploy is a pull, a build and a restart:

```bash
ssh web@192.168.1.60
cd /srv/mole && git pull --ff-only
pnpm --dir landing install --frozen-lockfile
pnpm --dir landing build
pm2 restart mole-landing
```

## Customize

- **Domain:** `MOLE_LANDING_URL` in `landing/.env`; `astro.config.mjs` falls back
  to `https://mole.luqueee.dev` when the file declares nothing.
- **Repo URL:** `https://github.com/Luqueee/mole` is hardcoded in `Nav.astro`,
  `Hero.astro`, and `Footer.astro`.
- **Theme:** the "subsurface map" palette and type scale live in
  `src/styles/global.css`. The surface is pale paper; anything underground —
  terminal blocks, the tunnel band in `Tunnel.astro`, platform cards, the
  footer — uses the dark `subsoil` tokens, where the CLI's FORWARD/UNFWD/INFO
  colours live.

## Gotcha: prettier and whitespace-sensitive markup

Prettier reflows the contents of `<pre>` and `whitespace-pre-wrap` elements,
which silently corrupts the rendered terminal output, the install commands and
the command-table chevron. Four spots are pinned with `{/* prettier-ignore */}`
— in `TerminalBlock.astro`, `Install.astro`, `Platforms.astro` and
`Commands.astro`. Don't remove them.
