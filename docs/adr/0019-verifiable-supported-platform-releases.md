# ADR-0019: Verifiable releases for supported service platforms

- **Status:** Accepted
- **Date:** 2026-08-18

## Context

Coagent had no official artifact or provenance path. A public release must let a
user connect a downloaded binary to a reviewed tag without making unsupported
platform promises. The service installer and native write sandbox currently
support Linux and macOS, while archive tools differ across those hosts.

## Decision

Tags matching `v*` produce `CGO_ENABLED=0` binaries for Linux and macOS on amd64
and arm64. A network-independent shell entrypoint cross-compiles with the pinned
Go toolchain; a Go helper creates normalized tar/gzip archives and a sorted
SHA-256 manifest, avoiding host-tar differences. Each archive contains only
`coagent` and `LICENSE`.

After the pinned module cache is prepared, the release entrypoint disables Go
module and toolchain resolution, so archive assembly itself has no network path.
The GitHub release workflow uses least-privilege OIDC to sign the checksum
manifest with Sigstore keyless signing, creates GitHub build-provenance
attestations for the archives, and publishes all verification material. Workflow
actions are pinned to commit SHAs. Release identity is reproducible for the same
source, version and pinned toolchain; identity across different toolchains is not
claimed.

## Consequences

Users can verify checksums, signing identity and provenance independently. The
local builder can reproduce workflow artifact bytes without GNU tar or
credentials once dependencies are cached. Windows and container images are
deliberately not official release artifacts; adding either requires a supported
install/runtime contract and a new decision. Repository owners must still
protect release tags and enable the GitHub security features described in
`SECURITY.md`.

## Alternatives Considered

Publishing unsigned binaries was rejected because a checksum hosted beside an
artifact does not establish who produced either. Building archives with shell
`tar` was rejected because GNU and BSD metadata behavior diverges. Container and
Windows artifacts were rejected until their service lifecycle and sandbox
semantics are supported rather than merely cross-compilable.
