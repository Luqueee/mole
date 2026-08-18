import type { APIRoute } from "astro"

// Served as a real route rather than a file in public/ so the sitemap line is
// derived from `site`: a copy pasted host would point crawlers at whichever
// machine the file was written on.
//
// One wildcard rule, and it allows everything, which includes the AI crawlers:
// there is nothing here that is not meant to be read. A `Disallow` list would
// have to name paths this site does not have.
export const GET: APIRoute = ({ site }) => {
  const sitemap = new URL("sitemap-index.xml", site).href

  return new Response(`User-agent: *\nAllow: /\n\nSitemap: ${sitemap}\n`, {
    headers: { "content-type": "text/plain; charset=utf-8" },
  })
}
