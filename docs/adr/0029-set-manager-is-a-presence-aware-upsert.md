# ADR-0029: `set_manager` is a presence-aware upsert

- **Status:** Accepted
- **Date:** 2026-08-23

## Context

The manager configuration tool presents one operation for adding or changing a
manager, but its update path constructs a new `ManagerEntry` from tool arguments
and replaces the old entry. The tool cannot express every manager field, so an
ordinary update can silently remove service-topic customization, timing policy,
Whisper configuration, and any future field omitted from the schema. Resolved
defaults can conceal the loss by making the rewritten config valid with
different behavior.

Manager updates also cross the restart-apply protocol, while Telegram manager
IDs already carry immutable routing and forum identity under ADR-0028. The
mutation contract must distinguish omission from explicit false, zero, empty,
and clear values without turning each field into a separate restart-producing
tool.

## Decision

`set_manager` remains one upsert operation. A new manager ID creates an entry;
an existing ID applies a presence-aware patch to the complete raw unresolved
entry.

- Omitted fields preserve existing raw values. Present fields replace their
  complete value, including the full allow-list.
- Every current YAML-editable manager field is exposed by the tool. Empty
  service-topic strings and zero timing values retain their established
  reset-to-default meaning; `whisper: null` removes Whisper configuration.
- Manager ID, driver, and ADR-0028 forum identity cannot change in place. A
  token reference may change because a secret variable name is not bot
  identity; once a v1 service-topic binding exists, startup rejects a different
  Telegram bot account.
- A patch that changes no raw value is valid and still restarts the daemon. It
  is the supported retry after external Telegram capabilities or permissions
  are repaired.
- Verdict summaries quote the manager ID and name only the operation and changed
  fields, never field values. Unknown, duplicate, incorrectly cased, or
  ambiguously nullable JSON input is rejected before staging.
- Service-topic name/icon settings apply when coagent creates or replaces a
  topic. An existing topic keeps its remote customization across restarts;
  session-topic icons remain creation defaults for future session topics.

The existing suspend, pending-apply marker, daemon restart, and verdict protocol
is unchanged.

## Consequences

- Updating one setting cannot delete another setting merely because the model
  omitted it, and a future `ManagerEntry` field is preserved before the tool
  schema learns how to edit it.
- Creation and update share one tool, but creation requirements are semantic:
  only the manager ID is universally required by the wire schema.
- The tool parser and config operation must carry field presence explicitly;
  ordinary Go zero values are insufficient.
- The tool can disable and re-enable managers without deleting their config.
- No-op calls intentionally pay the normal backup and whole-daemon restart cost.
- Config-file application and manager startup health remain separate outcomes;
  startup errors continue to surface through status.
- A person can rename or re-icon an existing service topic in Telegram without
  coagent restoring the configured values on the next restart.
- Schema coverage tests must fail when a new YAML-editable manager field is not
  represented by the tool.

## Alternatives Considered

- **Keep whole-entry replacement and expose every field.** Rejected because a
  future field would silently restore the same destructive behavior until every
  caller and schema was updated in lockstep.
- **Use separate tools for token, allow-list, target, enablement, and optional
  settings.** Rejected because it expands tool selection ambiguity and turns one
  conversational edit into several daemon restarts without improving the merge
  boundary.
- **Treat every zero value as preserve.** Rejected because false, empty, and zero
  are legitimate explicit settings or reset signals; omission must be distinct.
- **Add a general JSON merge-patch or optional-value framework.** Rejected
  because manager mutation has one daemon caller and one config operation. A
  manager-specific patch keeps presence semantics at the owning boundary.
- **Add a manager-only restart tool.** Rejected because a no-op `set_manager`
  already supplies the required repair retry through the established
  restart-apply protocol.
