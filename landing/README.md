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

## Design and content

- **Identity:** the desktop app's industrial palette, square panels, Space
  Grotesk and JetBrains Mono. Fonts are self-hosted through Fontsource.
- **Styling:** Tailwind v4 utilities for layout, responsive variants, and states.
  Theme tokens and shared typography live in `src/styles/global.css`; custom
  CSS handles illustrated browsers, paths, and spatial effects.
- **Scenes:** `Hero.astro` places a local browser in front of a perspective
  tunnel. `DiscoveryScene.astro` maps three selectable remote services to their
  matching localhost previews. These are conceptual illustrations, not the app UI.
- **Connection journey:** `TunnelJourney.astro` explains connect, discover, and
  forward with explicit stage buttons. On screens at least 1024×950 with motion
  enabled, a CSS-sticky scene advances through these stages as the user scrolls.
  Shorter screens and reduced-motion users get the normal-flow button experience.
- **File transfer:** `TransferScene.astro` moves an illustrated file to a remote
  inbox on demand and announces completion. It uploads nothing. Reduced motion
  skips the travel animation and shows the received state immediately.
- **Motion:** GSAP and ScrollTrigger move the hero's spatial layers, the tunnel
  packet, and closing artwork. Core explanatory copy is never hidden for an
  entrance effect. Reduced-motion preferences disable scroll choreography.
- **Installation:** `Install.astro` switches the whole stage between Desktop
  and Terminal while keeping the independent daemon explanation visible. `#cli`
  activates the terminal stage, including on direct entry. Mode and OS tabs
  support arrows/Home/End. Copy buttons select text if clipboard access fails.
  Both installation paths remain readable without JavaScript.
- **Desktop availability:** no public `desktop-v*` bundle was found during this
  redesign. The desktop card links to source and releases instead of inventing
  a download. When desktop bundles are published, update this card to link to
  the desktop release, not an unrelated CLI release.
- **Content:** `src/pages/index.astro` composes the scenes and owns FAQs.
  Repository links use `https://github.com/Luqueee/mole`.
- **Social preview:** `public/og.png` is a committed 1200×630 capture of the hero.
  Regenerate it from the local page with fonts loaded and reduced motion enabled.
- **SEO and deployment:** canonical URLs, JSON-LD, robots, sitemap, Search
  Console verification, and environment-gated Umami analytics are preserved.
  `MOLE_LANDING_URL` continues to determine the public site URL.
