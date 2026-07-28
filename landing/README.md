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

The site is static — no Astro adaptor needed. **Vercel** is the deploy target,
configured by `vercel.json`.

### Vercel

1. Push the repo to GitHub.
2. In Vercel: **Add New → Project**, import the repo.
3. Set **Root Directory** to `landing/`.
4. Vercel auto-detects Astro via `vercel.json` — Build Command `npm run build`,
   Output Directory `dist`.
5. Add your custom domain under **Settings → Domains**.

## Customize

- **Domain:** `site` in `astro.config.mjs` is `https://mole.luqueee.dev`.
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
