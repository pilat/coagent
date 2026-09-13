# ADR-0057: Reuse user-command activation for full configuration edits

- **Status:** Accepted
- **Date:** 2026-09-12

## Context

The daemon must allow a model to replace the application configuration from an ordinary project session, but only when the user explicitly delegates that action. The existing `/budget` path already creates durable, one-turn authority from a manager-owned user command. Its routing and lifecycle are generic, while budget-specific code still owns validation and durable consumption. Adding configuration editing without a reusable settlement contract would make each future gated mutation reimplement expiry, cancellation, consumption, and replay behavior.

Configuration is also daemon state, not an ordinary project file. The generic `edit` tool is deliberately confined by project write boundaries, while configuration changes already have a validation, backup, pending-apply, restart, rollback, and post-restart verdict protocol.

## Decision

We reuse the durable user-command activation mechanism for `/config` and make its settlement contract operation-neutral. A gated mutation must validate its grant against the exact session, tool, command, and tool-call identity; consume it once on successful mutation; and settle it through the shared expiry, cancellation, and replay rules.

We add one daemon-side `config_edit` tool. It is advertised in eligible root sessions, declares `/config`, accepts a complete replacement YAML document, validates it before writing, and applies it through the existing configuration restart protocol. It does not broaden ordinary file-tool access and does not replace the existing configuration project, CLI manager, control socket, onboarding, or structured configuration tools.

## Consequences

- Future user-authorized mutation tools have one authority lifecycle instead of budget-specific cleanup logic.
- `/config` can be used from ordinary root sessions while schedules, subagents, copied transcript text, and model-authored text cannot create authority.
- The full-document path gets the existing config backup, marker, rollback, and restart guarantees.
- Invalid configuration is rejected without changing the file.
- Configuration edits still restart the daemon; this decision does not introduce live in-memory reload.
- The implementation must coordinate grant settlement with the suspended config-apply call and prove idempotency across crashes and verdict replay.
- The new tool is intentionally separate from ordinary `edit` and from the structured config tools because its trust boundary and apply lifecycle differ.

## Alternatives Considered

- **Give ordinary `edit` an exception for `~/.coagent/config.yaml`.** Rejected because it weakens the general project write boundary and bypasses the configuration service's validation, backup, marker, rollback, and restart protocol.
- **Dynamically register `config_edit` only after `/config`.** Rejected because activation-dependent tool schemas complicate running sessions and prompt caching; the established mechanism keeps the tool advertised while granting authority only for the user-authorized turn.
- **Keep grant settlement inside each tool.** Rejected because it makes future gated mutations repeat durable lifecycle logic and risks stranded or reusable grants.
- **Add a second live-reload configuration holder.** Rejected because the existing restart-based apply protocol already preserves sessions and provides rollback semantics; live reload would add a separate lifecycle model without a required benefit.
