# ADR-0040: Tool calls use declared ordered scheduling

- **Status:** Accepted
- **Date:** 2026-09-01

## Context

One assistant response may contain several native tool calls, and the built-in
`batch` tool offers the same shape to models that do not emit native multiple
calls reliably. Both paths started every call concurrently. That reduced model
round trips for independent reads, but also let an adversarial or mistaken model
race edits, shell commands, shared clients, durable mutations and pending
external-call protocols. Prompt wording could encourage useful batching but
could not make execution safe.

The runtime must preserve one ordered result or exact pending owner per call,
including across failure, suspension and restart. Tool implementations know
whether concurrent invocation is safe; the model, provider adapters and generic
session loop do not. MCP annotations are hints rather than a trusted concurrency
contract.

## Decision

Every `tool.Tool` declares `ParallelSafe() bool`. Coagent schedules native calls
and admissible nested `batch` calls through one neutral `internal/toolexec`
component.

The executor partitions input order into maximal contiguous parallel-safe
stages and singleton barrier stages. It admits at most four callbacks from one
session at once through a rolling window. Every call in the current stage is
attempted; a typed or Go error, panic or suspension prevents every later stage
from starting. Results and explicit skips retain assistant call order. Context
cancellation stops new admission and remains responsible for every started
callback until it returns.

The initial parallel-safe declarations are `read`, `ls`, `glob`, `grep`,
`webfetch`, `todoread`, and `task`. `task` opts in to preserve native foreground
scatter/gather; its durable child lifecycle and all other specialized tool
protocols remain with their existing owners. MCP and every unlisted tool are
serialized.

`tool.Result.IsError` distinguishes typed failure without parsing text or
metadata. The error bit persists on tool-result messages, with legacy rows read
as non-errors. Session commits the complete non-pending result/skip set for one
assistant turn atomically so a persisted failure cannot be separated by a crash
from its decided skips. A fallback `batch` with a nested failure returns its
combined partial result with `IsError=true`, allowing the outer scheduler to
apply the same fail-stop rule.

Native multiple calls are the preferred model-facing form. `batch` remains a
compatibility fallback and keeps its restrictions on nested, activation-only,
skill, unknown and pending-external calls. Provider adapters retain their own
wire contracts; in particular, Anthropic receives one user message containing
the complete ordered `tool_result` group.

## Consequences

Independent allowlisted calls share a model turn and execute concurrently while
mutations and unknown capabilities have deterministic barriers. A model cannot
create a mutation race merely by emitting calls together or wrapping them in
`batch`. Failure and suspension become ordered control boundaries, and restart
cannot execute a call whose durable sibling outcome already skipped it.

Adding a tool now requires an explicit concurrency-safety choice. `true` is a
strong implementation contract, not a synonym for read-only; tests must keep
the allowlist deliberate. The fixed limit is per session, so the existing
sixteen-runner admission bound still permits a theoretical process-wide peak of
64 parallel-safe calls. No new global governor or configuration is introduced.

The durable error bit and atomic result-set insertion add a migration and a
cross-row transaction. Started callbacks that ignore context may still delay
stop or shutdown; detaching or forcibly timing them out is a separate lifecycle
decision. Parallel mutations, resource keys and trusted MCP declarations remain
deferred.

## Alternatives Considered

- **Trust the model and strengthen prompts.** Rejected because prompt compliance
  is not a safety boundary and providers may emit conflicting calls.
- **Serialize every call.** Rejected because independent reads would continue
  paying unnecessary model-round and wall-clock latency.
- **Classify tools as read/write/external.** Rejected because read-only does not
  prove concurrent implementation safety, while `task` is a safe concurrent
  mutation required for scatter/gather.
- **Derive path or resource locks from arguments.** Rejected for the first
  version because a static opt-in captures the read-heavy gain without building
  a universal lock manager.
- **Remove `batch`.** Rejected because it remains useful for models that cannot
  express native multiple calls reliably.
- **Add daemon-global tool admission.** Rejected until evidence shows the fixed
  four-per-session bound causes process-wide contention.
