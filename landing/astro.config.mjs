// @ts-check
import { existsSync } from "node:fs"
import { defineConfig } from "astro/config"
import tailwindcss from "@tailwindcss/vite"

// The host that serves the site declares itself in landing/.env. This file is
// not a module Vite transforms, so `import.meta.env` is empty here and a bare
// `process.env` would only see what the shell exported; `loadEnvFile` puts the
// file into `process.env`, which is also what feeds `import.meta.env` in the
// components. A variable already exported wins over the file.
if (existsSync(".env")) {
  process.loadEnvFile()
}

// https://astro.build/config
export default defineConfig({
  site: process.env.MOLE_LANDING_URL ?? "https://mole.luqueee.dev",
  output: "static",
  compressHTML: true,
  vite: {
    plugins: [tailwindcss()],
  },
})
