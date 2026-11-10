# ADR-0089: MCP server instructions follow tool visibility

- **Status:** Accepted
- **Date:** 2026-11-10

## Context

An MCP server may return `instructions` in its `initialize` result. They are hints
on how to use its tools, and the protocol suggests adding them to the system
prompt. Coagent discarded the whole initialize result from its first MCP client
(`31643ed`), with no recorded reason, so no session ever saw any server's
instructions. A server that drives a real computer puts essential rules there:
the coordinate space, what a "screen changed" reply means, and when it may be used.

These instructions are written by the server, and ADR-0064 treats MCP tool output
as data, not authority. They also enlarge every request of every session that
carries them.

## Decision

The MCP client keeps `instructions`, truncated to 8 000 characters of server text.
When it truncates, it appends a marker and logs one warning per client. The
system prompt's tools section, built from the session's filtered registry, gets a
`# MCP SERVER GUIDANCE` block with one delimited entry for each server that has at
least one tool in that registry. Entries are sorted by server name. The
instructions therefore follow tool visibility: the browser server's instructions
reach only the `browser` subagent, and those of every other server reach the
sessions that see its tools. The preamble calls the text guidance from an
operator-installed server, separate from MCP tool output. That guidance does not
override system, user, project or task instructions.

## Consequences

- Servers that put essential rules in their instructions now work as their authors
  intended.
- Each session that sees a server pays up to 8 000 characters of prompt for it.
  Output is deterministic, so the prompt cache holds between turns.
- The root never sees the browser server's instructions. Its delegation is guarded
  by coagent's own static guidance instead ([ADR-0088](0088-browser-surface-is-the-configured-mcp-server.md)).
- Very large announcements are still carried inline. Folding them into an
  on-demand catalog is follow-up work.

## Alternatives Considered

- **Pass instructions to the browser subagent only.** Rejected: the discard is a
  bug for every server, and the visibility rule covers the browser case without
  special logic.
- **Also pass the browser server's instructions to sessions that can spawn
  `browser`.** Offered and declined: the root learns the browser only through
  coagent's static texts.
- **No cap.** Rejected: a server could add tens of thousands of characters to every
  request.
- **A catalog the model opens on demand.** Deferred: it is the right shape for very
  large announcements, but more than this fix needs.
- **Treat instructions as untrusted output.** Rejected: they describe the server's
  own tools, like tool descriptions, which already reach the model unwrapped.
