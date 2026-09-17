# ADR-0060: Telegram service topics own daemon management

- **Status:** Accepted
- **Date:** 2026-09-14

## Context

The built-in CLI manager grew from a first-run helper into a second chat
transport over the Unix control socket. It coupled terminal rendering, streamed
model traffic, daemon installation and update, provider bootstrap, structured
configuration tools, and a masked `request_secret` protocol. The model could
initiate the secret ceremony but could not observe its value, so correctness
depended on suspension, socket pushes, terminal interception, replay, and
restart behavior spanning several packages.

This surface remained unreliable while duplicating a conversation path already
provided by Telegram. The later `/config` activation protocol also removed the
need for a privileged `sys:coagent` project: any ordinary root can receive the
configuration tool, but a real manager-owned user message beginning `/config`
is the durable authority for one apply. Keeping the old CLI, bootstrap, secret
protocol, and socket mutations as fallbacks would preserve their complexity and
failure modes without a remaining product role.

Multiple Telegram managers still require independent transcripts and delivery
ownership. They may share daemon knowledge and project memory, but a manager
must never consume another manager's input or output. The Telegram service topic
already provides a stable management surface for each configured manager.

## Decision

Daemon management conversations live in each Telegram manager's service topic.
Every configured manager owns exactly one live root session there. These roots
have distinct manager ownership and transcripts, and share one hidden ordinary
project at `<projects_root>/sys_coagent`. A durable management-surface session
attribute selects embedded coagent instructions and service-topic routing. The
project's `hidden` flag controls discovery only and grants no tools or authority.

The live-root invariant is enforced atomically in durable storage per manager.
Startup ensures the root and reconciles its current service-topic binding before
output delivery begins. Delivery for a management root always resolves the
manager's current service topic and never creates or deletes an ordinary session
topic. `/clear` replaces the root while preserving that binding; management
roots are excluded from manager-level kill flows.

`config_edit` is the only model-facing configuration mutation. It remains
advertised on eligible roots, but can apply only under the existing durable
activation created by a manager-owned `/config` user command. Its validation,
backup, restart, boot validation, rollback, and verdict replay remain intact.
Literal credentials are accepted, and `${VAR}` resolution from manually
maintained secret files remains available.

The built-in CLI manager, bare-command onboarding, automatic installation and
update, deterministic provider bootstrap, structured configuration tools,
`request_secret`, and their compatibility paths are deleted. Bare `coagent`
prints usage. Installation and lifecycle operations are explicit `coagent
daemon ...` commands.

The same-user Unix socket retains JSON-RPC framing, greeting/readiness behavior,
and the read-only `status` method. It carries no model traffic, pushes,
configuration mutations, secrets, or restart operation.

The logical project identity `sys:coagent` and its special controller authority
are deleted. A forward migration adds the discovery-only hidden-project field
and deletes the old CLI project's complete durable graph rather than adopting
it into Telegram sessions. The physical `sys_coagent` directory is preserved
for the new hidden project.

This decision supersedes ADR-0007, ADR-0008, ADR-0009, ADR-0022, and ADR-0049.
ADR-0057 remains accepted for its `/config` activation and whole-file apply
protocol; its statement that the CLI manager and system project remain is
superseded here.

## Consequences

- Three configured Telegram managers have three management roots and one shared
  project ID. Project memory is shared; transcripts, input, output, and delivery
  ownership remain manager-specific.
- A daemon without a valid model and Telegram configuration has no interactive
  recovery wizard. The operator edits configuration manually and runs explicit
  lifecycle commands.
- The control protocol becomes small enough to describe as a status API rather
  than a private chat transport.
- Hidden projects must be filtered from every project discovery path, including
  `/new`, recent projects, and `/spawn`. Hidden state must never become an
  authorization condition.
- Old CLI conversation history, schedules, pending work, memories, and related
  state are intentionally lost on upgrade. They are not meaningful to adopt
  into one of several independently owned Telegram management sessions.
- Literal credentials can appear in `config.yaml` and its backups. Operators
  who want indirection must maintain `${VAR}` values themselves.
- Removing the automatic update path also removes its socket restart guarantee.
  The retained system-service layout and explicit installer must stand on their
  own without bare-command update behavior.

## Alternatives Considered

- **Keep the CLI as an optional fallback.** Rejected because its chat, push,
  secret, and restart protocols would remain production contracts and retain
  most of the code being removed.
- **Keep only a deterministic first-provider and first-manager bootstrap.**
  Rejected because a partial wizard still owns credential input, config writes,
  daemon readiness, and recovery. A badly functioning bootstrap is worse than
  an explicit manual prerequisite.
- **Reuse `sys:coagent` as the management authority.** Rejected because `/config`
  already has explicit user-command authority. Project identity would duplicate
  that boundary and make a discovery concern security-sensitive.
- **Share one management session across all Telegram managers.** Rejected
  because manager ownership is the delivery boundary; sharing a transcript
  would mix inputs and outputs between independently configured managers.
- **Retain removed JSON-RPC methods as no-ops.** Rejected because compatibility
  has no supported client and would conceal stale callers instead of failing
  them with method-not-found.
