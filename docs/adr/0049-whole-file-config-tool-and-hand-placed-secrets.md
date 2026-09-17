# ADR-0049: Configuration is edited as one whole-file tool; secrets are placed by hand

- **Status:** Superseded by [ADR-0060](0060-telegram-service-topics-own-daemon-management.md)
- **Date:** 2026-09-08

## Context

The reserved `sys:coagent` configuration session exposed eight structured,
per-field mutation tools (`set_provider`, `remove_provider`, `set_manager`,
`remove_manager`, `add_model`, `remove_model`, `set_default_model`,
`set_model_tags`) plus an in-session `request_secret` ceremony: the tool
suspended the model, the CLI opened a masked terminal prompt, the value travelled
once over the socket, and the model learned only the variable name. Both existed
to enforce a `${VAR}`-only credential discipline — `config.yaml` holds a
reference, never a literal secret, resolved from `~/.coagent/secrets.*` at load.

Two costs accumulated. The tool surface grows one tool + op + schema per config
section. The secret ceremony spread a request/resolve push protocol across
`ctl`, `cli`, `sessionevent`, the daemon, and `cmd/coagent`, plus masked-terminal
interception.

The tension is that the `${VAR}` machinery is two separable things. The
**enforcement** (refusing a literal) and the **interception** (the masked
in-session entry) are ceremony a single-user, self-hosted daemon with an operator
at the terminal does not need. But the `${VAR}` **resolution** is load-bearing:
it keeps hand-placed secrets out of `config.yaml` and its backups, out of the
captured bash environment, feeds MCP-server env expansion at acquire, and drives
log redaction. The decision must drop the ceremony without dropping the
resolution that does the real security work.

## Decision

- **One `coagent_config` tool replaces the eight structured tools**, registered
  only on the configuration session (the [ADR-0022](0022-reserved-coagent-configuration-project.md)
  boundary is unchanged). `read` returns the raw `config.yaml` with `${VAR}`
  **unresolved** — resolving on read would hand the live key to the model.
  `write` takes the whole YAML and runs it through the **existing**
  `Stage → Commit` (backup + marker) `→ restart → boot-validate → rollback`
  pipeline via a new `ReplaceConfig` op; it returns `tool.ErrSuspend` exactly as
  the retired tools did. The daemon's own crash-safety (a broken config rolls
  back, never boot-loops) is fully preserved; only the per-field surface
  collapses.
- **Remove the `${VAR}`-only enforcement** (`checkCredential`). A literal
  credential now passes validation. The `${VAR}` resolution machinery is kept
  unchanged.
- **Remove the in-session secret ceremony**: the `request_secret` tool, the
  masked-prompt/socket interception, and the request/resolve push protocol.
  Secrets are placed by hand in `~/.coagent/secrets.*` and referenced as
  `${VAR}`; mid-session the model instructs the operator to edit that file.
- **Keep the deterministic first-run bootstrap** (driver choice + key → write).
  It never used the interception and remains the survivable first-run path,
  because the config session cannot run the LLM until a provider+model exists.

This retires the in-session secret-entry clauses of
[ADR-0007](0007-cli-manager-onboarding.md) and the `request_secret` clause of
[ADR-0022](0022-reserved-coagent-configuration-project.md). The rest of both —
CLI-manager onboarding over the unix socket, and the reserved `sys:coagent`
project as the config-tool registration boundary — stands.

## Consequences

- Configuring the service is editing a file: one tool, whole read / whole write,
  through the same restart-safe pipeline. A new config section needs no new
  tool — just fields in the YAML.
- A literal credential **can** now land in `config.yaml` and its backups; nothing
  enforces the `${VAR}` indirection. Guidance (tool description, onboarding
  skill) still steers to `secrets.*`. Accepted.
- Diff-only guards are gone (manager driver/forum immutability, "only provider",
  "name the replacement default"). Referential integrity is still enforced
  centrally by `ParseAndResolve`, with blunter messages.
- Whole-file writes lose `config.yaml` comments and key ordering on re-marshal —
  already true for every structured apply.
- The removal is cross-cutting: `request_secret` plumbing spanned
  daemon/ctl/cli/sessionevent/cmd, and the eight tool IDs were bound by ~20
  scenario tests whose config-apply coverage (suspend/restart/rollback) is
  re-authored against the new tool rather than deleted.
- A session checkpointed mid-call to a retired tool immediately before the
  upgrade is not swept with the deliberate orphan notice; it self-heals via
  generic transcript repair on next input. Accepted, no migration sweep added.

## Alternatives Considered

- **Edit `config.yaml` with the generic `apply_patch`/`edit` tool.** Rejected: a
  raw write bypasses validate/backup/restart/rollback; config apply must
  `suspend → restart` while a generic edit returns immediately; and generic tools
  exist in every session, whereas config mutation is config-session-only.
- **Patch (`old`/`new`) input instead of whole-file.** Rejected: the config is
  tiny and the model already holds it from `read`; whole-file has the fewest
  moving parts.
- **Kill `${VAR}` resolution too ("without remainder").** Rejected: resolution
  keeps secrets out of config/backups/bash-env, feeds MCP env expansion and log
  redaction; removing it buys nothing for the goal and forces literals
  everywhere.
- **Two tool IDs (read non-external, write external) so `read` can batch.**
  Rejected: discards the single-tool design for a cosmetic `batch` limitation the
  config session never hits.
- **A legacy external-call name set to sweep pre-upgrade dangling calls
  cleanly.** Rejected: generic transcript repair self-heals on a single-user
  tool, and the "ask again" notice is stale advice once `request_secret` is gone.
