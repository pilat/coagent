# ADR-0004: MCP server registry moves from YAML to SQLite with project scoping

- **Status:** Accepted
- **Date:** 2026-08-07

## Context

MCP servers live in `config.yaml` under `mcp.servers`, loaded once at daemon startup and frozen for the process lifetime. There is no per-project scoping: every session in every project gets the same server set, and adding a server means editing YAML and restarting the daemon. The agent cannot self-serve ("add this MCP to the project" is impossible mid-conversation).

Two existing properties make a dynamic registry cheap: the MCP pool already keys subprocesses by `hash(server config × workdir)` and self-manages lifecycle via refcounts and idle TTL, and the session tool stack is rebuilt from scratch on every loop iteration (`BuildStack` → `AcquireForWorkDir`, released on `Close`). Nothing caches the server *list* — only the running subprocesses.

The secrets invariant constrains storage: credentials live only in `~/.coagent/secrets`, resolved in-memory; they must not land in SQLite.

## Decision

MCP servers move to a SQLite table with project scoping: `project_id = NULL` rows are global, non-NULL rows belong to one project; a session's server set is global rows merged with its project's rows (project wins on name collision). The `mcp.servers` YAML section is removed outright — the project is pre-production, no migration path is provided.

Rows are managed by built-in session tools (add / remove / enable / disable / list) with an explicit `global | project` scope parameter; rows carry an `enabled` flag so a server can be switched off without deleting it. The tools are excluded from subagent tool sets. `env` values are stored with `${VAR}` references **literally**; resolution against the in-memory secrets map moves from config-load time to acquire time.

Changes propagate through the natural per-iteration stack rebuild: the next loop iteration reads the current rows and the pool starts/retires subprocesses by hash. Additionally, remove/disable evict the server's pooled subprocess (immediately when idle, on last release when a live stack still references it) instead of waiting out the idle TTL. No hot-attach into a live registry and no same-run availability: every change takes effect from the next run, and the tool result says so.

## Consequences

- Per-project MCP servers exist; the agent manages them conversationally without daemon restarts.
- The pool needs one addition — eviction by server name for remove/disable; everything else rides the existing hash/refcount/TTL machinery.
- The DB is the single source of truth; "where did this server come from" has one answer. A planned future source (reading a repo-local MCP config) must merge into this same resolution path.
- A user who pastes a live token into chat gets it persisted as plaintext in SQLite; only the tool description enforces the `${VAR}` discipline. Accepted: the same user already has full filesystem read via Bash.
- Global servers are managed from any session with `scope: global`; the planned onboarding project may later become the curated place for that, but nothing depends on it.

## Alternatives Considered

- **YAML for globals + DB for project rows.** Rejected: two configuration systems for one concern, permanent "which source defined this server" ambiguity, and the global side would still need a restart to change.
- **Full CRUD surface (controllerapi methods + Telegram `/mcp` command).** Rejected as the primary interface: heavier API and UI work than the need justifies; the in-session tool covers the real flows. A read-only listing can be added later if wanted.
- **Hot-attach into the live session registry.** Rejected: the stack already rebuilds every iteration, so attach machinery (tracking extra acquisitions for release, mutating a built registry) buys one iteration of latency at real complexity cost.
- **Same-run availability via suspend-and-wake (sleep mechanism) or self-notification re-entry.** Both rejected after design review: suspend-and-wake required parametrizing the scheduler's wake path, generalizing pending-call tracking, and an interrupt contract; the notification variant still added a transport for what amounts to UX sugar. Next-run availability plus pool eviction covers the need with near-zero machinery.
