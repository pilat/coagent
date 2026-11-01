# ADR-0081: The assistant forwards the user's words and the host relays work answers into `#0`

- **Status:** Accepted
- **Date:** 2026-11-01
- **Amends:** [ADR-0074](0074-assistant-is-an-ordinary-session-with-a-desk-head.md),
  [ADR-0076](0076-flat-chat-with-a-focus.md),
  [ADR-0078](0078-assistant-input-is-never-human-authority.md),
  [ADR-0079](0079-chat-shows-where-each-message-comes-from-and-every-model-step.md),
  [ADR-0080](0080-assistant-administers-work-at-open-and-hears-direct-exchanges.md)

## Context

The owner runs many long-lived sessions and writes to one chat. On `#0` the assistant
retold everything, in both directions.

- **Into work.** `send` and `open` carried the assistant's rewrite of the user's message,
  plus a host-attached quote of it (ADR-0074's "every hand-over carries the user's current
  message verbatim"). A short reply like «да, делай» reached the session wrapped in the
  assistant's words.
- **Out of work.** The chat delivers only the focused session's rows (ADR-0076). On `#0`,
  then, the assistant's retelling was the *only* copy of a work answer. The brief first
  assumed the chat already showed those answers; the code and the live database showed it
  did not. In the two weeks to 2026-11-01, root 744 received 30 work-answer events and
  produced a retelling for 28 of them. About three carried a decision or a mismatch worth a
  line of the assistant's own.

The research behind this (`ai/research-human-throughput.md`,
`ai/research-orchestrator-market.md`) names that pattern as the failure. Retelling loses
provenance and introduces errors, and the market keeps direct entry into a session while
the coordinator routes. ADR-0074 also forbade silent turns, so each event cost a line in
`#0` whether or not it said anything new.

The constraints:
- **Authority.** ADR-0078/0080 define human input as `source = 'user'` without
  `from = assistant`. Only human input creates activation grants (slash commands, ADR-0031)
  or lifts a fired budget.
- **Transport-neutral.** The design must work on any text messenger: no reply threading, no
  edits.
- **Visible routing.** The user must see where each message went, so a misroute can be
  corrected with `/switch`.

## Decision

**`forward(session)` moves the user's own words.** It is an assistant Do tool whose only
parameter is the target session.
- The host takes the activation's newest accepted human input and enqueues its
  `raw_content`, byte for byte, into the target as `source = 'user'` without `from`. The
  model chooses the address only; since it never writes the text, it cannot paraphrase.
- An activation with no human input (an event turn) gets an error.
- The forwarded row carries `forwarded_by` (the assistant root) and `forwarded_input_id`
  (the source inbox row). It has the observer reference that every human input to an owned
  work root already gets, and delivery key `forward:<input id>`, so it is idempotent per
  target.
- `forward` never moves the focus (ADR-0077 is unchanged).

**A forwarded message is human input, with two exceptions.**
- **Slash commands.** Text starting with `/` is never forwarded. The tool refuses it, and
  `EnqueueTx` refuses any forwarded input starting with `/` before any other check. Slash
  activations are the only grants, so a forward creates none, and the ADR-0031 hole stays
  closed.
- **Fired budgets.** A forward into a root whose budget has fired is refused with the
  existing `ErrBudgetParked` text. Lifting a checkpoint stays an explicit `set_budget` with
  a receipt in `#0`. A misroute must never spend money.

Otherwise the forwarded words are the user's. The assistant's *own* words (`send`, `open`)
remain non-human, as ADR-0078 says.

**The host writes the routing line.** In the same transaction as the work-inbox row, the
store appends `→ #N <project · title> (forwarded)` to the assistant root's output log, keyed
`forward:<input id>:<N>`.
- The glyph is the switch prelude's. The trailing word keeps it from reading as a focus
  change, and the switch header itself is unchanged.
- A forward counts as answering the user. A generic `tool.Result.AnswersInput` marks it, so
  a routing-only user turn may end with no further text.

**`send` and `open` deliver only the assistant's text.** The attached quote of the user's
words is removed. When the exact words matter, the assistant forwards after `open`/`send`.

**The host relays observed work output into `#0`.** Inside the work root W's commit, at the
point where ADR-0075 enqueues the observer event, each observed final of the window, and
each observed failure notice, is copied into the observing assistant root's output log.
- **What is copied.** It is W's own user-visible output row, verbatim: after a completion
  check that is the confirmed candidate text.
- **Keys and ordering.** The key is `relay:<W output id>`, so re-reports and replays never
  duplicate. The copy is written before the assistant runs.
- **One predicate.** The relay is skipped only when the manager's chat focus is W at that
  moment, which covers instructions, forwards, direct exchanges and reported schedules
  alike.
- **Header.** On `#0` the row is delivered as `#W <name>` followed by the content. The
  header is rendered at delivery, never stored, as ADR-0079 requires for work rows.
- **Watermark.** W's watermark does not move, so the same final shows again on `/switch W`.
- **Non-accepting observer.** An observer that cannot take output (killed, stopping) is
  skipped, as its event is.
- **Desk.** The desk lists relay and forward rows among its conversation lines.

**Silence is allowed when nothing is owed.** The daemon builds assistant sessions with
`SilentFinish`.
- **The rule.** An empty `stop` then ends the run with no output, provided no accepted human
  input awaits an answer. "Awaits" runs from acceptance until an `AnswersInput` result
  commits. An empty reply to a pending second-look candidate confirms that candidate.
- **User turns.** A user turn without a forward still ends with text, enforced by the
  unchanged empty-stop nudge path. `internal/session` reads only the option and the result
  field, never the agent type.
- **The prompt.** On a work answer, failure or reported schedule, the assistant does its
  bookkeeping and speaks only when the session asked a question (one line framing the
  decision, then forwarding the user's reply), when the result does not match what was
  asked, or when the user asked to be told. Its own schedules and host notes are always
  spoken.
- **Routing comes first.** On a user message the first decision is whether it is the
  assistant's or a session's. A continuation of a session's work is forwarded; new work,
  questions, administration and multi-project requests are the assistant's.

## Consequences

- **The user reads sessions in their own words, and the assistant speaks only when it adds
  something.** On the measured fortnight, about 25 of 28 retellings disappear. The lines
  that remain are decisions, mismatches and requests.
- **ADR-0074 is amended twice.**
  - "There are no silent turns" now holds only for user turns that were not forwarded.
  - "Every hand-over carries the user's current message verbatim" is withdrawn: hand-overs
    carry the assistant's text, and the user's words travel only by `forward`.
- **ADR-0076's rule that a chat shows only the focused session's rows gains one
  exception.** `#0` also carries copies of observed work output, marked by origin.
  Non-focused sessions themselves are still never touched.
- **ADR-0078's human-input predicate is unchanged.** Forwarded input is human, but it never
  lifts a budget and never carries a slash command. ADR-0078's rule that only `set_budget`
  lifts a checkpoint for the assistant now covers forwarded input too.
- **ADR-0079's header rule extends to relayed rows on `#0`.** Assistant rows themselves
  still have no header.
- **ADR-0080's per-exchange cost changes shape.** "Each exchange costs one assistant
  activation and one short line in `#0`" becomes one activation and, usually, no line. The
  user reads the session's answer itself in `#0` unless they were in that session. The
  accumulation step recorded there stays for part 3.
- **Misrouting is the owner's accepted risk.** A forward can land in the wrong session. The
  user sees `→ #N … (forwarded)` and can `/switch`, and the budget refusal bounds the cost.
- **Duplicates are accepted.**
  - A relayed final shows again on `/switch W`, and W appears in the `#0` digest.
  - A yield final read live in W is relayed if the focus left W before the closing final.
- **Some silent turns lose a check.** A silent finish gets no second look, because the
  completion nudge fires only on non-empty text. The prompt tells the assistant to do its
  bookkeeping before finishing. Part 3 replaces the second look anyway.
- **Attachments stay behind.** An attachment sent on `#0` reaches the assistant only as its
  caption, so a forward carries the caption and the file stays behind.
- **Edge cases.**
  - With two user messages in one activation, `forward` takes the newest.
  - A crashed forward-only turn that resumes without new input owes text, and the resumed
    run is a new activation: `forward` refuses the message accepted before the crash, so
    the assistant hands it over with `send` in its own words.
- **Relay and forward rows are written outside the target's own commit** and emit no session
  events of their own. The chat loop picks them up through the existing wake paths or its
  five-second fallback.

## Alternatives Considered

- **`forward(session, text?)` with an optional text.** Rejected: any text parameter
  reopens paraphrase. The point is that the model cannot write the words.
- **Keep the quote attached to `send`/`open`.** Rejected: the session receives two
  versions of one request and cannot tell which wins. The quote was also skipped whenever
  the assistant's text happened to contain the words.
- **Let the assistant write the `→ #N` line.** Rejected: a line the model can forget is not
  a guarantee. A daemon row in the forward's own transaction cannot be missing, and it
  cannot follow the answer.
- **The line written as a tool `DirectMessages` receipt.** Rejected: it commits with the
  tool result after the work-inbox commit, so a fast answer could precede it, and a crash
  between the two commits leaves a forward with no line.
- **A forward that lifts a fired budget like direct typing,** optionally with a
  `· budget released` note. Rejected by the owner: a misrouted «продолжай» would spend
  money.
- **User turns always end with text, forwards included.** Rejected by the owner: every
  forward would add a redundant «Передал».
- **Two relay rules** (decision 6 as first written): always relay answers to the
  assistant's instructions and forwards, and relay direct exchanges only while the focus is
  `#0`. Rejected for one predicate. The first rule replayed answers the user had read live
  after "open it and connect me", and the second hid direct answers that finished while the
  user was in a third session.
- **Relay only the committing final.** Rejected: a closing final often only points back at
  an earlier yield final, which `#0` never showed.
- **Relay `messages.content`.** Rejected: after a completion check the user-visible final
  is the confirmed candidate in the output log, not the post-nudge transcript text.
- **Store the `#W name` header in the relayed content.** Rejected for the reason ADR-0079
  gives: titles change after rows are written.
- **Advance W's watermark on relay.** Rejected: it would also mark W's step messages as
  read, so a later `/switch W` would skip them.
- **Nudge once before every silent finish,** to keep the second look. Rejected for now: an
  extra model call per silent event, for machinery that part 3 removes.
- **An assistant-type branch in `internal/session` for silent finishes.** Rejected:
  ADR-0074 keeps the loop free of type compares. A session option and a generic result
  field express the rule.
- **The same glyph without `(forwarded)`.** Rejected by the owner: it reads as a switch, so
  the user would think the focus moved and type into the wrong place.
