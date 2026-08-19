# ADR-0007: Onboarding runs as an AI chat through a built-in CLI manager on a unix socket

- **Status:** Accepted
- **Date:** 2026-08-08

## Context

A newcomer today must hand-author `config.yaml` (providers, models, Telegram IDs) plus a secrets file before anything works. A full TUI configurator was built to solve this (branch `wip-config-onboarding`: ~5.3k lines, 35 files, its own staging state model over 31 mockup screens) and rejected: the surface was untestable by one maintainer, and it duplicated — in a second UI stack — configuration logic the daemon must own anyway. The daemon deliberately opens no network listener, so any local control path must ride a unix socket. Meanwhile coagent's own architecture already has the right shape for a user-facing surface: managers (Telegram) driving sessions via `controllerapi.Controller`.

## Decision

The `coagent` binary's onboarding is a thin deterministic bootstrap followed by an AI-led chat:

- Bare `coagent` checks/install/updates the daemon, then ensures at least one provider (prompt name + key, sent over the socket), then opens a chat.
- The chat is served by a **built-in, always-on CLI manager** — a peer of the Telegram manager inside `internal/managers`, using the same `controllerapi.Controller` contract, with the unix socket (`~/.coagent/daemon.sock`) as its transport. It is not a config entry; it exists whenever the daemon runs.
- On start the manager get-or-creates the reserved logical project `sys:coagent` in the `sys_coagent` directory through an internal-only controller marker, then binds the CLI chat to a single root session there. User project names cannot contain `:`, and user project creation cannot claim `sys_coagent` ([ADR-0022](0022-reserved-coagent-configuration-project.md)).
- Onboarding guidance (Telegram setup etc.) is a built-in skill, not a dedicated agent type. Its full instructions are automatically active in this session's system prompt; success does not depend on the model discovering and invoking the skill.
- Secrets never transit the chat: a `request_secret` tool suspends the session (`ErrSuspend`), the CLI shows a local masked prompt, the value travels once via a socket `set_secret` op, and the model sees only the variable name. The tool exists only on the CLI channel — Telegram sessions can never carry it.
- Provider, model and manager mutation tools exist only on the root session whose project has both the reserved `sys:coagent` name and canonical `<projects_root>/sys_coagent` path. A numeric SQLite session ID is not an identity because clear/recreation may replace it. `request_secret` additionally requires `channel=cli`, because it needs a person at the terminal. Telegram roots, ordinary project roots and subagents never receive these tools.

## Consequences

- One UI stack dies: no TUI, no mockup-fidelity testing burden. The testable surface is the deterministic bootstrap plus per-tool table tests.
- The CLI channel is a durable product asset (a local chat to the daemon), not one-shot onboarding scaffolding.
- The socket protocol must carry session traffic (send/stream/suspend round-trip), not just status — it becomes the daemon's local API and the later seed of an external MCP adapter.
- A terminal is required for daemon-wide provider, model and manager configuration, including creation of new secrets.
- If the user pastes a secret into the chat as text, nothing can stop it landing in session history (accepted residual risk; the skill warns and redirects to the prompt).
- Out of scope, accepted: sending Telegram messages via socket ops, multi-session CLI, web UI.

## Alternatives Considered

- **TUI configurator (built, then rejected).** 31 screens, its own staging/dirty state model, ~5.3k lines untestable without a QA team; duplicated daemon-owned logic in a client. Parked on `wip-config-onboarding`.
- **Plain CLI wizard for everything.** Deterministic and testable, but walking Telegram setup (forum topics, chat capture, troubleshooting) as a static question tree is exactly the interaction an LLM does better; for an AI tool, AI onboarding is also the product demo.
- **Web UI.** Rejected earlier for release timing; would also require the daemon to open a listener.
- **Hand-edited YAML as the onboarding.** The status quo being solved; unsurvivable first run.
