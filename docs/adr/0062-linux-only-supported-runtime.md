# ADR-0062: Linux-only supported runtime and release platforms

- **Status:** Accepted
- **Date:** 2026-09-19

## Context

Coagent's security boundary depends on the native process filesystem sandbox,
service lifecycle, and the tests that prove those behaviors. Supporting Linux and
macOS required separate Bubblewrap and Seatbelt implementations, separate
systemd and launchd service paths, platform-specific rooted-filesystem behavior,
and a two-OS CI and release contract. The two environments did not provide the
same security semantics, and keeping both paths correct increased the chance that
security improvements would drift or be validated unevenly.

The project is a single-operator unattended daemon. Its useful security contract
is the one that can be implemented, exercised, and released consistently. Linux
already provides the chosen Bubblewrap runtime, service lifecycle, and complete
verification path.

## Decision

Coagent supports Linux only. Official release artifacts are `linux-amd64` and
`linux-arm64`; CI and release verification run on Linux; the service installer
registers only the systemd unit; and the native write/shields process boundary
uses only the Linux Bubblewrap backend.

The command entrypoint rejects any non-Linux platform that can build the existing
command package before starting the process guardian, loading configuration,
installing a service, or running the daemon. Unsupported build targets such as
Windows are not made cross-compilable or releasable as part of this decision.

On Linux, `sandbox.enabled: false` remains an explicit operator choice. The
sandbox is still enabled by default, and disabling it retains the existing
unconfined behavior and makes session shields unavailable. This platform decision
does not strengthen the Linux boundary into confidentiality, network isolation,
or multi-tenant security.

Earlier ADRs that record the former Linux-and-macOS contract remain unchanged as
historical records, including ADR-0009, ADR-0019, ADR-0040, ADR-0042, and
ADR-0044. This preserves the repository's requested historical text even though
the current architecture and release contract supersede those platform premises.

## Consequences

- One native sandbox implementation, one service manager, one CI platform, and
  one release matrix receive ongoing security and behavioral verification.
- The release builder and workflow publish only two platform tuples, reducing
  artifact, provenance, and documentation drift.
- Manually built binaries for buildable non-Linux targets fail with an explicit
  unsupported-platform error rather than silently running without the Linux
  sandbox backend.
- Existing Linux operators retain the sandbox configuration switch; this change
  does not force Bubblewrap when an operator explicitly disables confinement.
- macOS users receive no supported runtime, installer, CI, or future release
  artifact from the current branch. Removing any existing external release is an
  operator action outside this repository change.
- Historical ADRs still contain the former platform contract by design; current
  documentation and architecture text must not copy that contract into active
  guidance.

## Alternatives Considered

- **Continue supporting Linux and macOS.** Rejected because the Seatbelt and
  launchd paths create a second security and lifecycle implementation whose
  semantics cannot be kept identical to Linux at acceptable maintenance cost.
- **Remove official macOS artifacts but leave macOS runnable.** Rejected because
  it leaves an ambiguous support promise and permits a manually retained binary
  to outlive the verified security contract.
- **Reject every non-Linux target at compile time.** Rejected as the primary
  behavior for buildable Unix targets because an explicit runtime error is more
  diagnosable. Unsupported targets that cannot compile remain outside the build
  contract rather than receiving compatibility work.
- **Make the Linux sandbox mandatory.** Rejected because operators retain an
  explicit `sandbox.enabled: false` escape hatch, and the existing session-shield
  rule already records the stronger boundary's prerequisite.
