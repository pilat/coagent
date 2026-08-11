# ADR-0011: Durable session input and ledger-projected waits

## Status

Accepted

## Context

Normal messages previously had two transports: an idle session was resumed with a prompt, while a running session received an in-memory “steering” message. Subagent follow-ups could therefore be acknowledged and then lost when the child runner finalized before draining that memory queue. Generic suspension also produced Telegram text saying the session was sleeping, even when the actual obligation was a subagent or another external call. Models compensated by calling `sleep` and polling subagent results, creating extra wakes and synthetic resume chatter.

## Decision

- Persist every normal user or agent message in the SQLite `session_inbox` FIFO before acknowledging it.
- Create a subagent's session, completion link, and initial inbox message as one transaction; a recoverable child is never prompt-less.
- Let the session, not the controller, promote input into the append-only transcript at an agent-loop boundary. Receipt time is preserved as message time.
- Use in-memory state only for runner ownership and typed external-call results; it is never the data transport for normal messages.
- Serialize each child's follow-up/terminal boundary. Follow-up accepted before terminalization stays in the current activation. Follow-up after terminalization starts a new activation only after the previous outcome was durably delivered to the parent. Continuation is available for both foreground and background children; once a foreground task result has resolved its original call, later activations report asynchronously through `subagent_event`.
- Give each reusable child an internal monotonic `activation_seq`. Completion delivery carries that sequence and its atomic CAS requires an exact match, so an at-least-once signal from an older activation cannot consume the delivery slot of a re-armed activation. The sequence is persistence protocol, not a model-facing child or round ID.
- Foreground subagents suspend the harness and currently join with an `all` policy. Background subagents complete asynchronously and wake the parent through their durable completion ledger. Model-authored sleep or result polling is not a subagent join mechanism: `task` plus `sleep` in one tool batch is rejected before the sleep side effect, and daemon-bound sleep is rejected while a child completion remains pending.
- Publish structured waiting events projected from exact pending-sleep rows (`metadata.tool_call_id`) and foreground-subagent ledgers. Metadata-free one-shot schedules are future input, not a wait. Managers render the projection and do not guess from transcript text or a generic suspended flag.
- Carry the loop's actual suspension discriminator through `session.RunDaemon`; do not reconstruct it from producer ledgers after the run, because those ledgers may have been created during that same run.
- Give every standalone scheduled occurrence a deterministic delivery identity. Commit its fingerprint and transcript mutation (including the entire fresh-context reset) together in SQLite; an acknowledgement retry returns `applied=false` and does not rerun the model or republish controller output. Identity/payload conflict fails closed.
- `/stop` parks the root and all live descendants, resolves outstanding calls with a stopped result, removes exact pending-sleep rows, cancels accepted-but-unconsumed input, and preserves recurring plus standalone one-shot schedules. Sessions and child history remain available; a stopped child resumes only through explicit follow-up. `/kill` remains destructive.

## Consequences

Accepted input survives runner exit and daemon restart. The enqueue-versus-terminal race no longer loses subagent follow-ups, and Telegram cannot label a subagent join as sleep. The transcript remains the single execution journal after inbox promotion; the inbox is only a short-lived acceptance ledger with explicit terminal states.

The architecture adds a persistence table, an internal child-activation sequence, and conditional SQLite transitions that linearize child terminalization/re-arming against inbox writes, completion delivery, and `/stop`; no process-local mutex spans persistence calls. Configurable `any` and `fail-fast` foreground joins are deferred; the only current join policy is `all`.
