# ADR-0018: Lazy LSP installs are pinned, verified, and atomically published

- **Status:** Superseded by [ADR-0024](0024-project-owned-lsp-servers.md)
- **Date:** 2026-08-17

## Context

Coagent starts language servers unattended. When a server is absent, the LSP
package installs it on first use. The original implementation mixed discovery,
version selection, downloading, archive extraction, and publication in one file.
Several installers used mutable versions (`@latest` or an unversioned package),
and direct release downloads were piped through shell tools into their final
paths without an integrity check. An interrupted or unexpected download could
therefore leave a partial executable, and platform selection could install an
amd64 binary on arm64.

Lazy installation is valuable for a headless agent: requiring every supported
language server up front makes installation substantially heavier. Keeping it
means the unattended executable supply chain must be explicit and reviewable.

## Decision

Coagent keeps lazy LSP installation with three trust classes:

- An executable already present on `PATH`, or in a valid legacy coagent-owned
  path, wins. It belongs to the user's existing toolchain; coagent neither
  replaces it nor enforces its version.
- Go, npm, and RubyGems installs name exact versions. They are populated in
  private temporary locations and become visible only after their expected
  launch target has been validated. Package registries and their clients remain
  responsible for package transport and integrity.
- Direct release artifacts name an immutable release and one of four supported
  tuples (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`). Coagent
  bounds the compressed download, verifies a pinned SHA-256 digest, applies a
  second bound to the decompressed executable, extracts only the exact regular
  executable entry with Go's archive readers, and publishes it atomically.
  Unsupported tuples fail instead of falling back to a different architecture.

Package-manager trees use versioned roots under `~/.coagent/bin/lsp`. Temporary
files and directories live beside their destination so publication stays on one
filesystem. A failed install cleans up its temporary state and never deletes or
replaces an existing invalid destination. A concurrent loser accepts the winner
only after validating the same expected launch target. Final roots and direct
executables cannot be symlinks; package-manager binstubs may be symlinks only
when their resolved target remains inside the validated versioned root.

Versions and direct-artifact digests are source constants. Updating an LSP
version is a reviewed code change that updates its version, artifact matrix, and
tests together.

## Consequences

- A network failure, checksum mismatch, malformed archive, or interrupted
  package install cannot expose a partial server as successfully installed.
- Direct downloads no longer require `curl`, `tar`, `gunzip`, `unzip`, or a
  shell pipeline, and wrong-architecture fallback is impossible.
- Fresh installs are reproducible at the selected-version level. Existing
  executables remain intentionally outside that guarantee.
- Updating language servers is manual. Old versioned package roots are not
  automatically removed; cleanup requires a separate retention decision.
- Pinned hashes detect changed release bytes but do not replace publisher
  signatures or a transparency log. The maintainer still trusts the upstream
  publisher when reviewing a digest update.
- npm and RubyGems packages occupy separate roots instead of sharing one prefix,
  trading some disk deduplication for atomicity and isolation.

## Alternatives Considered

- **Remove lazy installation and require every server on `PATH`.** This has the
  smallest coagent supply-chain surface, but makes the advertised multi-language
  LSP support an installation exercise before first use.
- **Vendor all supported servers with coagent.** This gives release-time control
  but greatly increases binary/distribution size, mixes many upstream licenses
  into every release, and still requires an update process for those artifacts.
- **Keep mutable package versions and only make writes atomic.** Atomicity avoids
  partial files but does not make an unattended install reviewable or
  reproducible.
- **Download and unpack complete archives with external tools.** This is shorter
  code, but depends on host utilities, expands more archive surface than coagent
  uses, and makes checksum-before-extraction and hermetic failure tests harder.
- **Verify signatures for every upstream.** Stronger where supported, but the
  selected projects do not expose one uniform signing mechanism. It can be added
  per upstream later without weakening the pinned-digest baseline.
