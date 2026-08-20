# ADR-0024: LSP servers are project-owned

- **Status:** Accepted
- **Date:** 2026-08-21

## Context

ADR-0018 added lazy coagent-owned language-server installs. Keeping those
installs correct required version pins, integrity verification, platform
artifacts, atomic publication, and an update process. That supply chain is
unrelated to the LSP client and makes code intelligence harder to understand
and maintain.

Projects already own their language toolchains. Their activated shell
environment is the authority for language-server versions, prerequisites and
configuration.

## Decision

Coagent discovers and starts language servers only from the activated project
shell PATH. It neither downloads, installs, pins, updates nor manages language
server binaries. If the required server is unavailable, the relevant LSP
operation reports that it is unavailable.

## Consequences

- LSP behavior follows the project's existing toolchain activation.
- Coagent no longer owns an LSP package-manager or artifact supply chain.
- Operators install and update any desired language server through their normal
  project or user toolchain.

## Alternatives Considered

- **Keep managed fallback installs.** Rejected because their maintenance and
  supply-chain surface are disproportionate to the LSP client feature.
- **Bundle language servers with coagent.** Rejected because it increases the
  distribution, licensing and update surface.
