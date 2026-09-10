# ADR-0051: Allow nested Bubblewrap in rootless outer namespaces

- **Status:** Accepted
- **Date:** 2026-09-10

## Context

The shields-down filesystem-write sandbox launches Bash through Bubblewrap. When
coagent develops coagent inside that sandbox, native Bubblewrap tests launch a
second Bubblewrap process. The outer rootless user namespace remaps host-root
files to the kernel overflow UID, and its read-only root bind also makes the
inherited `/proc` mount read-only. The inner process therefore cannot create its
UID map, while the launcher trust check cannot observe the host root ownership.

Skipping the native tests would hide the behavior that caused the failure, and
disabling the outer sandbox would make local verification materially different
from normal sessions. The fix must preserve the existing integrity boundary;
shields-up must remain filesystem-confined even when its runner is constructed
from a shields-down outer process.

## Decision

The shields-down Linux Bubblewrap prefix mounts a fresh `/proc` at `/proc` after
all other mount operations so a nested rootless Bubblewrap can create its user
namespace mapping without a later bind shadowing procfs. Launcher validation
accepts the kernel overflow UID as the remapped view of a trusted root-owned
Bubblewrap binary only when all of these conditions hold:

- the process is already in a valid non-initial user namespace;
- the canonical executable is outside every writable root; and
- the executable's filesystem is read-only.

UID 0 is accepted only in a valid initial user namespace; it is rejected when
namespace state is non-initial or cannot be established. Because namespace
translation hides host ownership, the overflow branch treats the outer
sandbox's read-only system mount as the provenance boundary rather than
accepting arbitrary non-root files. Writable roots under `/proc`, `/dev`, and
`/sys` are rejected. Shields-up filters procfs mounts by filesystem type and
never mirrors them through project or runtime aliases; its final procfs remains
private under its PID namespace, tmpfs root, and private `/proc`.

## Consequences

- Native Bubblewrap and filesystem-tool tests can run from a normal shields-down
  coagent session without being silently skipped.
- The outer session keeps its read-only system filesystem; the additional procfs
  is a mount-namespace detail and does not grant a writable host filesystem root.
- Trust is intentionally fail-closed when the overflow UID, user-namespace
  state, or read-only mount cannot be established.
- The launcher check now has a Linux-specific rootless-container branch that must
  remain covered by policy tests and the real Bubblewrap integration tests.
- Environments that cannot create nested user namespaces still fail loudly; this
  decision does not turn an unavailable backend into an unconfined process.

## Alternatives Considered

- **Skip native-sandbox tests inside an outer sandbox.** Rejected because it
  leaves the local handoff gate unable to exercise the production boundary and
  postpones failures until CI.
- **Disable the outer sandbox for test commands.** Rejected because local tests
  would run with different filesystem authority from ordinary agent work.
- **Accept any non-root launcher owner.** Rejected because a writable or
  user-controlled Bubblewrap binary would bypass the trust boundary.
- **Share the shields-down procfs with shields-up.** Rejected because shields-up
  intentionally creates a private PID namespace and `/proc`; its stronger
  filesystem policy must not inherit host-readable process state.
