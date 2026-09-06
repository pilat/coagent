# ADR-0044: Session shields confine project filesystem access

- **Status:** Accepted
- **Date:** 2026-09-06

## Context

Coagent is an unattended, single-operator daemon. Its normal sessions need the
operator's installed tools and broad read access, while the existing native
sandbox limits writes. That default is useful for trusted work but is too broad
when project content or another input might manipulate the agent into reading
same-user data outside the task.

A separate multi-user permission system would not solve this within one
operator account. Removing Bash, LSP, or MCP would also create an artificial
difference between tool surfaces without protecting in-process file tools. The
operator instead needs one understandable switch that narrows filesystem
authority for a session and all of its subagents.

## Decision

We add durable, operator-controlled **session shields**. Shields are down by
default. While down, file reads retain the daemon user's authority and the
default-enabled native sandbox continues to restrict writes. A manager may
raise or lower shields with host-handled `/shieldsup` and `/shieldsdown`
commands; the commands never enter model context. The state belongs to each
session row, changes for the complete root tree, survives restart and root
replacement, and is inherited transactionally by new subagents.

While shields are raised, built-in file access and session-owned Bash, LSP, and
stdio MCP processes are confined to the canonical project. The project is the
only writable user-data root. Configured writable paths, caches, host temporary
storage, captured `PATH` directories, and linked-worktree Git metadata outside
the project grant no exception. Process tools retain only a fixed, read-only,
OS-specific execution substrate for base executables, loaders, libraries,
resolver data, certificates, and devices. Raised sessions do not capture,
replay, or refresh shell activation.

Built-in tool classes stay registered. MCP tools retain their ordinary
discovery behavior and may be absent when a raised-policy server cannot start.
Project-local instruction sources use rooted
project access; global and marketplace instructions remain trusted daemon
inputs. The web fetch/search tools and network access keep their existing behavior.
Shields constrain filesystem authority; they do not promise project-data
confidentiality, environment-variable filtering, remote-side-effect control,
Unix-socket isolation, or multi-tenant isolation.

Raising shields fences and parks an active session tree through the existing
stop lifecycle before confirming the transition. It also retires old-policy MCP
processes. Lowering shields never expands a running tree: an active raised tree
must become idle before the command succeeds. Shields cannot be raised when the
native sandbox is disabled.

This narrows the filesystem grant in
[ADR-0042](0042-work-tree-sessions-write-the-main-repository-git.md) only while
shields are raised; shields-down work-tree sessions keep that decision intact.

## Consequences

- One durable state explains both in-process and subprocess filesystem access;
  Bash is not a privileged bypass around built-in file tools.
- A raised session can continue using common system commands, LSP, and MCP when
  their complete runtime fits the fixed substrate, but developer toolchains that
  depend on home directories, caches, host `/tmp`, or external Git metadata can
  fail normally.
- macOS Seatbelt requires exact read access to the filesystem root directory to
  launch a confined process. Top-level names remain enumerable there, but data
  and metadata below roots outside the project and execution substrate stay
  denied.
- A raised session can still transmit project data or secrets already present in
  its environment. Operators must not treat shields as a confidentiality or
  multi-user boundary.
- Raising shields interrupts active work and applies the full stop-tree cleanup,
  but reports the shield transition rather than a user-requested stop.
- Processes that deliberately detach from the owned process group may survive
  the first implementation's stop phase; stronger process ownership remains
  future work.
- The default-enabled sandbox becomes a prerequisite for the stronger state;
  explicitly disabling it preserves unconfined reads but makes `/shieldsup`
  unavailable.

## Alternatives Considered

- **Always confine reads to the project.** Rejected because ordinary sessions
  need the operator's installed tools and files, and coagent began as a
  single-operator agent.
- **Remove Bash, LSP, and MCP while raised.** Rejected because the filesystem
  boundary can apply consistently across process and built-in tools without
  discarding useful capabilities.
- **Allow configured read or write exceptions while raised.** Rejected because
  exceptions make the operator-facing guarantee dependent on ambient global
  configuration and can silently expose credentials or caches.
- **Mount the host filesystem read-only and deny selected secrets.** Rejected
  because a denylist cannot enumerate same-user sensitive data and makes the
  protection difficult to explain.
- **Provide a private `/tmp` immediately.** Rejected from the first version to
  keep the boundary small; compatibility can be added later without widening
  host access.
