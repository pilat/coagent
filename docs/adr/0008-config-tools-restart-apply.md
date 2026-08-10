# ADR-0008: Config mutations are root-session tools; apply = file write + daemon self-restart

- **Status:** Accepted
- **Date:** 2026-08-08

## Context

Onboarding and later reconfiguration need a mutation path for providers, models, and managers. A full snapshot-holder design (atomically swappable in-memory config, live reload, per-change verdicts) was built on `wip-config-onboarding` and rejected as complexity without need: coagent sessions already persist to SQLite and resume after a process restart by design, so avoiding restarts buys nothing. Separately, ADR-0004's MCP registry set a precedent: daemon-owned state mutated by plain built-in tools (`mcp_add` family) registered onto root sessions per iteration, with credentials passed only as `${VAR}` references.

## Decision

Configuration mutations follow the `mcp_tools.go` pattern, and applying them restarts the daemon:

- Provider/model/manager mutations are **plain daemon-side tools** implementing `tool.Tool`, registered per iteration onto **all root sessions** (temporarily — tightening the audience is future work), exactly like the MCP registry tools. No MCP loopback, no new plumbing.
- Providers, models, and managers **stay in `config.yaml`**; the daemon is the only writer. A mutating tool validates, takes a timestamped `config.yaml.bak`, writes atomically, and triggers a **self-restart**. A daemon that fails to boot on the new file rolls back to the last bak and starts on it. There is no in-memory snapshot holder and no live-reload path.
- A tool call whose answer arrives after the restart is completed via the session's normal suspend/resume: a persisted pending-apply marker lets the rebooted daemon deliver the verdict into the suspended session, channel-agnostically.
- Credential values never appear in tool arguments: tools accept only `${VAR}` references to existing secrets (creation of secrets is the CLI channel's `request_secret` path, ADR-0007).
- Tools must refuse to saw off the branch the daemon sits on: removing the last provider, or the default model without a replacement, is a rejected mutation, not a restart into a dead config.

## Consequences

- One semantic op layer serves every facade (socket bootstrap op, session tools); validation and secret handling live in one place.
- Config changes are visible to sessions only after a restart; running sessions checkpoint and resume — a brief, survivable hiccup rather than a hidden reload machinery.
- Every apply costs a restart and a bak file; bak retention bounds the pile.
- Any root session on any channel can reconfigure the daemon it runs on — acceptable for a single-owner tool, revisited when an admin boundary exists.
- The pending-apply marker and boot-time rollback become correctness-critical paths and need dedicated tests.

## Alternatives Considered

- **Snapshot-holder live reload (built, then rejected).** Atomic config swap, construction-capture migration, apply verdicts without restart — real complexity, and its only benefit (no restart) is worthless when sessions already survive restarts.
- **MCP loopback for config tools.** Exposing socket ops as an MCP server the daemon spawns against itself: subprocess lifecycle, serialization, pool TTL, and an extra hop for secrets — all to obtain what in-process registration gives for free.
- **Moving providers/models/managers into SQLite** (the mcpstore precedent). Deferred: the YAML file is fine, human-ownable, and the restart-based apply works the same either way; migrating storage now would churn schema without changing the contract.
- **Socket CRUD as the only mutation path** (no session tools). Rejected: the agent is the operator now — onboarding and later reconfiguration happen in chat, and a socket-only path would leave the model unable to act.
