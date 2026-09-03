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

Subsystem contracts:

- `internal/tunnel` owns one SSH connection shared by the forwarded ports. Reconnects must be
  cancellable, bounded, and free of leaked channels, listeners, and goroutines. Host-key checking
  and both Unix and Windows authentication paths are security-sensitive.
- `internal/discover` must distinguish a failed remote probe from a successful probe with no
  matching listeners. Discovery should be based on actual remote TCP listeners and retain the
  documented reserved-port behavior.
- `internal/config` and `cmd/mole` define the public configuration, command, flag, help, exit-code,
  and daemon-lifecycle contracts. Preserve the documented precedence and compatibility when adding
  options.
- `.github/workflows` is production infrastructure: the landing artifact is built and checked
  before publication, and deployment reaches the private server through Tailscale and SSH without
  exposing credentials.

When a concern is uncertain, explain the evidence and its assumptions instead of presenting it as
a definite defect. Avoid repeating a finding already covered by the repository's formatters or
linters unless the configuration makes it a real correctness or security issue.
