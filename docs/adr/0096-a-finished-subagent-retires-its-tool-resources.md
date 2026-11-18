# ADR-0096: A finished subagent retires its tool resources

- **Status:** Accepted
- **Date:** 2026-11-18
- **Amends:** [ADR-0063](0063-explicit-filesystem-boundary.md)

## Context

Production accumulated dozens of roughly 100 MB `mcp-remote` processes over a
few hours until the OOM killer intervened. The tool-resource cache retained
shell snapshots and MCP clients by session ID across activations. A finished
subagent usually never ran again, but its resources remained until stop, kill,
clear or daemon shutdown. Each new subagent added another full MCP process set.

Retaining resources amortizes startup within an assignment, but retaining them
after every completed assignment makes process growth unbounded for a live tree.

## Decision

Retire a subagent's own tool resources when its link becomes terminal through
completion, error or failed resume. The daemon retires that session ID through
the existing tool-resource owner after terminalizing the link and before
delivering its outcome. Retirement runs synchronously under the tree lock;
an idle lease closes immediately and a busy lease closes on release.

A yield keeps resources warm for the same assignment, as required by
[ADR-0071](0071-subagent-result-waits-for-its-own-background-work.md). Each descendant
retires through its own terminalization. Root resource lifetimes stay unchanged.

## Consequences

- MCP process sets per tree are bounded by live roots and open children rather
  than every subagent session that has ever run in that tree.
- A follow-up or re-armed subagent round restarts MCP and recaptures its shell
  snapshot, adding startup latency while preserving the session transcript.
- A local browser server loses its page between rounds. The child must navigate
  back from its transcript and recorded `task_state`; its action history remains.
- Roots retain one resource set each until stop, kill, clear or daemon shutdown.
  Creating more roots can still increase process counts.
- Retirement errors are logged without preventing completion delivery. The
  existing resource owner remains responsible for cleanup.

## Alternatives Considered

- **Idle reaper or timers:** rejected because the terminal link already gives a
  precise lifetime boundary without new machinery. ADR-0063 removed the pooled
  model and idle lifetimes retained by
  [ADR-0041](0041-mcp-tool-catalogs-outlive-idle-clients.md) and
  [ADR-0045](0045-session-bound-mcp-process-identity.md).
- **Retire at runner exit:** rejected because a yielded child's next activation
  continues the same assignment under ADR-0071 and should reuse its resources.
- **Exempt browser children:** rejected because every browser task would retain
  the leak. The child retains its transcript; no contract promises page survival.
- **Retire the child's subtree:** rejected because a terminal child can have a
  background grandchild whose idle lease must stay warm while yielded or between
  activations. That grandchild owns its own terminalization boundary.
