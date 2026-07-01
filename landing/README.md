# mole — landing page

Static landing page for [mole](https://github.com/Luqueee/mole), built with
Astro + Tailwind v4 + shadcn/ui. Output is a fully static site in `dist/`.

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

The site is static — no Astro adaptor needed. Configs for both platforms are
included; **Vercel** is the intended target.

### Vercel (recommended)

1. Push the repo to GitHub.
2. In Vercel: **Add New → Project**, import the repo.
3. Set **Root Directory** to `landing/`.
4. Vercel auto-detects Astro via `vercel.json` — Build Command `npm run build`,
   Output Directory `dist`.
5. Add your custom domain under **Settings → Domains**.

### Netlify (alternative)

1. Push the repo to GitHub.
2. In Netlify: **Add new site → Import an existing project**.
3. Set **Base directory** to `landing/`. `netlify.toml` handles the rest
   (build command `npm run build`, publish directory `dist`).
4. Add your custom domain under **Domain settings**.

## Customize

- **Domain placeholder:** `site` in `astro.config.mjs` is set to
  `https://mole.dev` — replace with your real domain.
- **Repo URL:** `https://github.com/Luqueee/mole` is hardcoded in `Nav.astro`,
  `Hero.astro`, and `Footer.astro`.
- **Theme:** terminal-dark palette lives in `src/styles/global.css`.
