# mole — landing page

Static landing page for [mole](https://github.com/Luqueee/mole), built with
Astro + Tailwind v4 — no UI framework, no client-side JS runtime. Output is a
fully static site in `dist/`.

## Develop

```bash
npm install
npm run dev      # http://localhost:4321
```

## Build

```bash
npm run build    # → dist/
npm run preview  # serve dist/ locally
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

- **Domain placeholder:** `site` in `astro.config.mjs` is set to
  `https://mole.dev` — replace with your real domain.
- **Repo URL:** `https://github.com/Luqueee/mole` is hardcoded in `Nav.astro`,
  `Hero.astro`, and `Footer.astro`.
- **Theme:** terminal-dark palette lives in `src/styles/global.css`.
