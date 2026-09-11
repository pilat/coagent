# ADR-0052: Model attempts retain finish evidence and bounded recovery

- **Status:** Accepted
- **Date:** 2026-09-10

## Context

The agent loop historically inferred completion from response shape: text
without tool calls ended a run, while tool calls continued it. Provider finish
reasons were normalized for compaction, but ordinary assistant messages did not
persist them and the loop ignored them. An output-limit response could therefore
be published as complete or execute a partially generated tool call. Session 165
also showed that returning the resulting tool error to the model could trigger a
second full-limit generation.

Reliable recovery requires retaining the paid response and its usage while not
replaying incomplete content. It must survive daemon restart, compose with the
one-shot root-tree budget, and preserve the existing tool execution contract.

## Decision

Every ordinary agent-loop model attempt stores its normalized finish type and
raw provider finish reason. Accepted attempts enter the provider transcript;
rejected attempts retain their complete parsed response and usage in immutable
conversation history but are excluded from every model, compaction, manager,
subagent-result and orphan-call projection.

`stop` is the normal completion evidence. A `length` attempt is rejected before
output or tool execution. The first `length` atomically adds one linked,
host-authored recovery input and retries silently from the clean provider
transcript; a repeated `length` ends in a durable error. `unknown` ends in a
durable error without retry. Actual calls returned with `stop` retain their
existing structural tool routing for provider compatibility, and a `tool_calls`
finish without calls uses the existing bounded empty-response recovery.

Rejected attempt insertion, iteration advancement, budget observation and the
selected recovery, error, or budget-park outcome commit in one session-store
transaction. A budget crossing wins over recovery or integrity error. Recovery
identity is an explicit message link, never message text or process memory, and
a later externally promoted model input supersedes it through durable inbox
provenance. Reused-subagent terminalization selects an integrity error only
from the current accepted-input boundary, never from an older assistant answer.

## Consequences

- A provider output limit can no longer masquerade as task completion or cross
  the tool-execution boundary.
- The first incomplete response still consumes its full provider allowance;
  this decision bounds only subsequent recovery.
- Lifetime usage and budget accounting include rejected attempts, while model
  context and manager output do not.
- Finish reason and response shape remain independent, so provider quirks do not
  erase termination evidence.
- The message store gains another explicit projection discriminator and every
  direct content reader must state whether rejected attempts belong.
- Compaction summarizer failures retain their existing no-retry, non-persisted
  protocol; this decision governs ordinary agent-loop attempts.

## Alternatives Considered

- **Treat `length` as a normal final response.** Rejected because partial text
  is not trustworthy completion and partial tool calls are not executable work.
- **Return a malformed call as an ordinary tool error.** Rejected for
  output-limit responses because it exposes incomplete calls to execution and
  permits another uncontrolled generation.
- **Keep the rejected response in provider input and ask the model to continue.**
  Rejected because an incomplete tool-call turn may violate pairing, repeats a
  large payload in context and makes partial-text reconstruction provider
  dependent.
- **Retry inside the LLM transport wrapper.** Rejected because a durable session
  transition must record usage, enforce the one-retry limit across restart and
  compose atomically with budget state.
- **Add central tool-schema or semantic validation.** Rejected as unnecessary
  for termination integrity; tools retain responsibility for their parameters.
- **Cancel streaming once arguments become large.** Deferred because legitimate
  payload sizes need a separate policy and non-streaming providers cannot share
  that mechanism.
