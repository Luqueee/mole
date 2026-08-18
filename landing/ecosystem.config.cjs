// The landing is a static build: `astro build` emits plain files and there is
// no server entry point to run. pm2's own static server is what serves them, so
// the host needs no nginx and the project needs no adapter.
//
// It binds every interface because the only thing in front of it is the
// Cloudflare tunnel on the same box; TLS and the public hostname belong there,
// not to this process.
module.exports = {
  apps: [
    {
      name: "mole-landing",
      script: "serve",
      cwd: __dirname,
      exec_mode: "fork",
      instances: 1,
      autorestart: true,
      max_restarts: 10,
      env: {
        NODE_ENV: "production",
        PM2_SERVE_PATH: "./dist",
        PM2_SERVE_PORT: process.env.PORT ?? "6768",
        // One page, no client-side router: a miss is a miss, not a rewrite to
        // the index.
        PM2_SERVE_SPA: "false",
      },
    },
  ],
}
