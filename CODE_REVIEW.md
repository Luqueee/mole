# Code review guide

Mole is a cross-platform Go CLI and daemon that forwards TCP ports over SSH. Its repository
also contains a static Astro + Tailwind landing site and GitHub Actions that deploy it to a
private server through Tailscale.

Review priorities:

- Correctness, security, reliability, and backwards compatibility come before style.
- Keep changes focused. Do not request unrelated refactors, speculative abstractions, or cosmetic
  rewrites.
- Treat SSH, networking, process lifecycle, credentials, CI permissions, and deployment paths as
  high-risk areas. Findings in those areas should include concrete impact and a practical fix.
- Preserve documented CLI behavior and cross-platform support.
- Prefer deterministic tests for changed behavior, especially cancellation, failure, reconnect,
  and cleanup paths. Do not require tests for changes that cannot affect behavior.
- The landing site must continue to produce static output and preserve its accessibility, SEO, and
  responsive behavior.

When a concern is uncertain, explain the evidence and its assumptions instead of presenting it as
a definite defect. Avoid repeating a finding already covered by the repository's formatters or
linters unless the configuration makes it a real correctness or security issue.
