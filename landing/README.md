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

GitHub Actions is the canonical deployment path. A push to `main` runs the Go
and landing checks, then publishes the generated static files through Tailscale
to `/home/web/mole/landing/dist` and reloads the `mole-landing` pm2 process as
the `web` user. The destination directory and process are intentionally owned
by `web`; the workflow does not need root access.

For an emergency manual copy of an already-built `dist/` directory:

```bash
rsync -az --delete dist/ web@websites:/home/web/mole/landing/dist/
ssh web@websites 'pm2 reload mole-landing --update-env'
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
- **Preview card:** `public/og.png`, 1200×630, a committed asset rather than a
  build step. It is the logs screenshot scaled onto the paper background:

  ```bash
  sips --resampleWidth 1200 public/mole-logs.png --out /tmp/og-scaled.png
  sips -p 630 1200 --padColor ECE7DB /tmp/og-scaled.png --out public/og.png
  ```

- **SEO:** `robots.txt` is a route, not a file in `public/`, so its `Sitemap:`
  line and every absolute URL in the head — canonical, `og:*`, the JSON-LD
  graph — come from `site` and cannot name the wrong host. `@astrojs/sitemap`
  emits `sitemap-index.xml`. A relative `og:image` is the defect this replaced:
  most crawlers do not resolve one.

## Gotcha: prettier and whitespace-sensitive markup

Prettier reflows the contents of `<pre>` and `whitespace-pre-wrap` elements,
which silently corrupts the rendered terminal output, the install commands and
the command-table chevron. Four spots are pinned with `{/* prettier-ignore */}`
— in `TerminalBlock.astro`, `Install.astro`, `Platforms.astro` and
`Commands.astro`. Don't remove them.
