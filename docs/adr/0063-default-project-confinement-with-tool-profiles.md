# ADR-0063: Default project confinement with developer-tool profiles

- **Status:** Accepted
- **Date:** 2026-09-23

## Context

The ordinary filesystem-write sandbox exposes the host filesystem read-only
and grants writes to the project, host temporary storage, the user cache, and
configured writable paths. Shell activation runs before confinement. Raised
session shields narrow the filesystem but sacrifice user toolchains and linked
worktree Git operations. This leaves a choice between broad ambient access and
an environment that breaks ordinary development work.

The replacement must restrict ordinary sessions while preserving a broad set
of installed Linux development tools. The operator wants explicit recipes for
resources each tool needs, with additional authority enabled persistently for
individual tools. Automatically guessing grants from repository configuration
would let untrusted input decide the boundary. Private replacement toolchains,
cache remapping and overlays add a second environment-management problem that
is not needed to describe allowed access.

## Decision

Sandbox-enabled sessions start from an allowlisted filesystem. The base grants
the canonical project RW, private per-project temporary storage RW, and a fixed
system execution substrate RO. Linked `/gwt` sessions also receive the main
repository's complete `.git` RW, including while shields are raised; the main
checkout is not granted. This accepts shared Git objects, refs, hooks and
configuration as the authority inherent in a linked worktree.

Developer-tool profiles contain separate mount, socket and network entries.
Each entry has a `basic` or `escalated` level. Mount entries independently select
RO or RW: selected shared caches and state may be basic RW. All basic entries
are enabled by default. Escalation adds only the escalated entries of selected
profiles. Global and project escalation sets are persisted through the existing
`/config` restart/rollback protocol and combined by union; project settings
cannot remove global grants. Repository content and ordinary model tool calls
cannot grant additional authority.

Profiles grant real resources. There is no overlay, private replacement cache,
automatic tool installer or path-discovery permission expansion. Definitions
and their supported layouts are reviewed against official tool documentation
and useful compatibility scenarios. Shared writable state can affect other
projects; this is an explicit tradeoff for compatibility.

Directory grants include their contents, including pathname Unix sockets.
Separate socket entries expose additional exact pathname sockets, but do not
claim to filter sockets already reachable through directory grants. Requiring
Landlock ABI 9 for strict pathname-socket control is rejected to retain older
Linux compatibility. Network namespaces hide host abstract sockets; public
egress and network exceptions are defined by
[ADR-0064](0064-embedded-transparent-sandbox-gateway.md).

Raised shields suppress all profile entries, basic and escalated. They keep
the base project/runtime/temporary/Git grants and public egress. Existing
stop-tree, subagent inheritance, restart and idle-only lowering semantics
remain. The explicit `sandbox.enabled: false` operator opt-out from ADR-0062
also remains; enforcement failures never select that mode automatically.

Shell capture, executable lookup, final Bash/LSP/MCP execution, built-in file
access and attachment rereads use the effective authority. Shell snapshots are
policy-bound; MCP clients have the same stack lifetime as their process policy.
Optional resource availability does not change authority or rotate a live
network generation: declarations grant anchored paths, while individual
operations validate and pin the objects currently at those paths.

This supersedes [ADR-0044](0044-session-shields-confine-project-filesystem.md)
and [ADR-0042](0042-work-tree-sessions-write-the-main-repository-git.md).
It retains ADR-0042's complete shared-Git grant and standard-layout constraint,
while removing ordinary access to the main checkout. ADR-0051's nested-rootless
launcher provenance and private-proc guarantees remain required; its old
host-root mount shape is no longer the ordinary policy.

## Consequences

- Ordinary tasks cannot read arbitrary same-user files merely because a tool
  needs access to its installed runtime.
- Basic remains useful through reviewed shared-state grants; it is not a
  promise that workloads cannot influence later host tool invocations.
- Escalated Docker or similar daemon access can deliberately delegate broader
  host authority. Socket mounts are not protocol-level authorization.
- Shields are understandable as removal of profile exceptions, while keeping
  temporary storage and linked-worktree Git functional.
- Arbitrary custom installation layouts and startup scripts can require
  operator-authored profile entries instead of silently broadening access.
- Inherited environment values, project data already in history, trusted daemon
  inputs and same-project shared temporary state retain their existing trust
  implications. This is not a multi-tenant boundary.

## Alternatives Considered

- **Keep broad reads and use shields for sensitive work.** Rejected because
  the ordinary default should provide protection without breaking toolchains.
- **Make every profile opt-in.** Rejected because common tools should work from
  the shipped basic catalog without configuring each project first.
- **Assign one level per profile, or make every RW entry escalated.** Rejected
  because a tool can need harmlessly scoped writable cache alongside sensitive
  daemon or credential access. Levels belong to entries, separately from mode.
- **Per-project overlays or private replacement caches.** Rejected because
  additional storage/remapping semantics make profiles unnecessarily complex.
- **Strict socket allowlists requiring a new kernel.** Rejected in favor of
  explicitly trusting the contents of mounted directories on supported older
  systems.
- **Deny parent Git metadata while shields are raised.** Rejected because
  ordinary worktree Git mutations require the shared object/ref store.
