# ADR-0007: Onboarding runs as an AI chat through a built-in CLI manager on a unix socket

- **Status:** Accepted
- **Date:** 2026-08-08

## Context

A newcomer today must hand-author `config.yaml` (providers, models, Telegram IDs) plus a secrets file before anything works. A full TUI configurator was built to solve this (branch `wip-config-onboarding`: ~5.3k lines, 35 files, its own staging state model over 31 mockup screens) and rejected: the surface was untestable by one maintainer, and it duplicated — in a second UI stack — configuration logic the daemon must own anyway. The daemon deliberately opens no network listener, so any local control path must ride a unix socket. Meanwhile coagent's own architecture already has the right shape for a user-facing surface: managers (Telegram) driving sessions via `controllerapi.Controller`.

## Decision

The `coagent` binary's onboarding is a thin deterministic bootstrap followed by an AI-led chat:

- Bare `coagent` checks/install/updates the daemon, then ensures at least one provider (prompt name + key, sent over the socket), then opens a chat.
- The chat is served by a **built-in, always-on CLI manager** — a peer of the Telegram manager inside `internal/managers`, using the same `controllerapi.Controller` contract, with the unix socket (`~/.coagent/daemon.sock`) as its transport. It is not a config entry; it exists whenever the daemon runs.
- On start the manager get-or-creates the reserved project `coagent` (the existing idempotent `CreateProject` path) and binds the CLI chat to a single root session there. No attaching to other projects' sessions in v1.
- Onboarding guidance (Telegram setup etc.) is a built-in skill, not a dedicated agent type; the model is the driver's catalog-recommended onboarding model.
- Secrets never transit the chat: a `request_secret` tool suspends the session (`ErrSuspend`), the CLI shows a local masked prompt, the value travels once via a socket `set_secret` op, and the model sees only the variable name. The tool exists only on the CLI channel — Telegram sessions can never carry it.

## Consequences

- One UI stack dies: no TUI, no mockup-fidelity testing burden. The testable surface is the deterministic bootstrap plus per-tool table tests.
- The CLI channel is a durable product asset (a local chat to the daemon), not one-shot onboarding scaffolding.
- The socket protocol must carry session traffic (send/stream/suspend round-trip), not just status — it becomes the daemon's local API and the later seed of an external MCP adapter.
- A terminal is required to create new secrets; from Telegram, config tools can only reference existing `${VAR}` names.
- If the user pastes a secret into the chat as text, nothing can stop it landing in session history (accepted residual risk; the skill warns and redirects to the prompt).
- Out of scope, accepted: admin-project semantics and tool gating beyond root-only, sending Telegram messages via socket ops, multi-session CLI, web UI.

## Alternatives Considered

- **TUI configurator (built, then rejected).** 31 screens, its own staging/dirty state model, ~5.3k lines untestable without a QA team; duplicated daemon-owned logic in a client. Parked on `wip-config-onboarding`.
- **Plain CLI wizard for everything.** Deterministic and testable, but walking Telegram setup (forum topics, chat capture, troubleshooting) as a static question tree is exactly the interaction an LLM does better; for an AI tool, AI onboarding is also the product demo.
- **Web UI.** Rejected earlier for release timing; would also require the daemon to open a listener.
- **Hand-edited YAML as the onboarding.** The status quo being solved; unsurvivable first run.
