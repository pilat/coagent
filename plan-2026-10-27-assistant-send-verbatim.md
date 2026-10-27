# Plan: the user's own words travel with a single assistant hand-over

## Goal

When the assistant hands work to a session in response to a user message, whether it opens new work
or continues an existing session, the session receives only the assistant's
paraphrase. `applyOpen` → `Store.Open` and `applySend` → `Store.Send` deliver `effect.text`
and nothing else. A constraint lost in the paraphrase ("схему пока не трогай", "ничего не
меняй") never reaches the session. The prompt's habit "quote their words where the exact
wording matters" is a request to the model, not a guarantee. The host will mechanically
append the user's message verbatim to such a hand-over, so the session always sees what
the user actually wrote next to the assistant's instruction. Opens matter most here: the
prompt makes a fresh session the default ("Right place before new place … Prefer a fresh
session"), so most hand-overs are opens.

## Decisions

1. **Only a turn with exactly one hand-over gets the quote.** A hand-over is an `open`, or a
   `send` to an existing session (`effect.handle == ""`). The quote is appended to that one
   hand-over when *all* of these hold for the committed turn:
   - the turn's input is a user message (`t.input.Source == sessionstore.InputSourceUser`);
   - the user text (decision 4) is not empty;
   - the turn recorded exactly one work effect of kind `open` or `send`. A `send` to a
     handle from an `open` in the same turn counts, so open + send-to-its-handle is two
     effects and gets no quote;
   - every `stop` in the turn targets the same existing session as the single `send`.
     Any `stop` in a turn whose hand-over is an `open` disables the quote.

   Why: the assistant splits a message that touches several projects so each session gets
   only its own part (prompt habit "Each project gets its own part"). Appending the whole
   message to every part would hand session B the instruction meant for A. Coding agents
   tend to act on any task they see, so a "for context only" label is not enough. The owner
   chose single-hand-over-only over "always attach with a context note". Traded away: in
   the split case a lost constraint is still possible, and the paraphrase is its only
   carrier.

2. **An `open` gets the quote too.** The quote does not replace the self-contained
   instruction that a fresh session needs (the prompt still demands one); it is an extra
   source for checking meaning and constraints. «Проверь Redis в Atlas, схему не трогай»
   loses its second half in an open exactly as in a send. The quote is appended to the
   open's first instruction, the `text` that `applyOpen` passes to `l.store.Open`. The
   `about` line is not changed.

3. **Hand-overs caused by non-user events never get the quote.** A turn that handles a
   work answer, a failure or a host note (`Source` is agent) has no user words to carry.

4. **Format: a fixed English label, then the message as a Markdown blockquote**, appended
   after the assistant's text with one blank line between:

   ```
   <assistant's instruction>

   The user's own words:
   > первая строка сообщения
   > вторая строка
   ```

   The quoted text is `strings.TrimSpace(t.input.RawContent)` with `\r\n` normalised to
   `\n`, split on `\n`. Every line gets a `> ` prefix (an empty line becomes `>`), so a
   multi-line or pasted message is visibly delimited. The label is English like every other
   host-written line; the owner chose English over Russian. The user text is not clipped:
   it is the user's own message, and the session inbox has no size limit on enqueue.

5. **No duplicate when the paraphrase already contains it.** If the assistant's text
   contains the user text (normalised as in decision 4) as an exact, case-sensitive
   substring, nothing is appended. This keeps verbatim forwards ("go" → "go") clean.

6. **A command is never quoted.** If the trimmed hand-over text starts with `/`, nothing
   is appended. Work sessions recognise commands only by exact match on the trimmed content
   (`internal/session/step_boundary.go`: `/status`, `/help`, `/schedules`, `/compact`;
   `internal/daemon/wake.go` `isReadOnlyCommand`, which decides on delivery whether a
   stopped session is parked or resumed). A suffix would turn a command into a prompt.

7. **The decision is made at apply time; the tape is unchanged.** Whether to quote depends
   on all of the turn's effects, so it is decided after the commit, when `apply` runs (it
   already has `t`). `effect.text` stays the assistant's text. The tape's `→ project (id)`
   lines (`commitTape` for sends to existing sessions, `applyOpen`/`applySend` for opened
   ones, all through `sendLine`) keep showing only the instruction, because the user's line
   is already on the tape as `user: «…»`. Only the text passed to `Store.Open`/`Store.Send`,
   and so the work session's inbox content, carries the quote.

8. **The prompt learns about it in one sentence.** In `registry.AssistantPrompt`, habit
   "Hand over something self-contained", add one sentence: when a turn hands work to a
   single session, new or existing, the host also attaches the user's message verbatim,
   and the instruction must still stand on its own without it. Why: otherwise the model
   keeps pasting the user's words itself (a duplicate that decision 5 only catches when it
   is an exact substring), and it must not write instructions that lean on the quote,
   because split hand-overs do not get one. The existing "quote their words" guidance
   stays: it is still the only carrier for split hand-overs and for constraints from
   earlier messages.

No ADR: this is a small, easily reversed host behaviour.

## Constraints

- Keep the meaning of `Store.Open`, `Store.Send`, `Store.deliver`, `instructTx` and the
  `Host` interface. Compose the final text before calling `Store.Open`/`Store.Send`.
- Do not change `effect.text`, `sendLine`, `commitTape`, the `about` handling or anything
  the desk renders.
- Do not change `stop` behaviour.
- Add no schema or migrations; nothing new is persisted beyond the inbox content itself.
- Follow `docs/coding-style.md` and the repo's comment rules (comments only for a WHY,
  at most two lines).
- Verification cadence per `CLAUDE.md`: focused `go test ./internal/assistant/ -run <Name>`
  while working; `make all` once at the end, run bare (never piped).

## Out of scope, accepted

- **Split hand-overs (one message → several sessions) carry no quote.** Owner's choice
  (decision 1).
- **Open + send to its own handle in one turn carries no quote.** These are two effects;
  the case is rare.
- **Two sends to the same existing session in one turn carry no quote.** Follows from
  "exactly one"; quoting both would duplicate the message.
- **Constraints said in earlier messages.** The quote carries only the current message.
  Earlier constraints are still the assistant's job to carry into the instruction (the
  prompt already says the limiting phrase "is often an earlier message than the go-ahead in
  hand").
- **A short user message that happens to be a substring of the paraphrase is not appended**
  (e.g. «да» inside «Да, поправь права»). The paraphrase then carries those words anyway.
- **Attachments and voice.** What gets quoted is whatever text the Telegram manager put into
  the assistant input (`RawContent`: text, caption, or voice transcript). This change does
  not forward files.
- **Telegram reply-to as routing evidence, competing "yes" handling, revoking a queued
  instruction, a dialog test corpus.** Discussed; the owner rejected or deferred each.

## Affected code

- `internal/assistant/loop_effects.go` — `apply`, `applyOpen`, `applySend`: decide once per
  turn whether to quote and which effect gets it (decision 1). Compose the delivered text
  (decisions 4–6) and pass it to `l.store.Open` / `l.store.Send`. `applyOpen` already
  receives `t`; `applySend` needs the turn or the precomputed decision, so follow that
  pattern. The tape lines these functions write (`sendLine(card, e.text)`) stay as they are.
- `internal/registry/assistant_prompt.go` — one sentence in the "Hand over something
  self-contained" habit (decision 8). `TestAssistantToolsMatchPrompt` checks call-shaped
  mentions in the prompt, so write nothing that looks like a tool call, such as
  `` `send(` ``.
- `internal/assistant/loop_test.go` — new tests and the deliberate updates listed under Task 1.
- `internal/daemon/assistant_recovery_test.go` —
  `TestHarnessScenario_AssistantShutdownAfterCommitKeepsEffects` sends the user message
  "send work"; the model makes one send "not applied" to an existing session; the test
  asserts `raw_content = 'not applied'` in the work session's inbox. That turn now quotes,
  so update the assertion to the exact composed string
  `not applied\n\nThe user's own words:\n> send work` (no `LIKE`).
  `internal/daemon` is a CI-only package: neither `make all` nor the assistant `-run` covers
  it, so run exactly
  `go test ./internal/daemon/ -run TestHarnessScenario_AssistantShutdownAfterCommitKeepsEffects`.
- **Verified: no other existing test changes.** The only two expected-value updates are the
  daemon test above and `TestAssistantLoopGuardAsksForFocus`. These stay unchanged:
  - `TestAssistantLoopNeverRebindsAnEarlierHandle`: the only existing single-open turn.
    Its user text "start" and open text "start" are saved by dedup.
  - The "go" → "go" tests (`TestAssistantLoopGuardGivesUp`,
    `TestAssistantLoopRoundCapIgnoresStrayCalls`,
    `TestAssistantLoopAnsweredSessionNeedsFocusUpdate`,
    `TestAssistantLoopGuardFailureCommitsTheReply`): also saved by dedup.
  - `TestHarnessScenario_AssistantOpenAndRetell` in `internal/daemon/assistant_scenario_test.go`
    is quoted, but asserts nothing about the instruction text. Run it once with its exact
    `-run` to confirm.
  - Tests calling `Store.Open`/`Store.Send` directly bypass the loop and are unaffected.
  - `internal/managers/telegram` has no assistant open or send scenario.
- **Verified: the desk does not show the quote.** The `cards()` about fallback
  (`i.raw_content`) is used only without an `assistant_work` row. Every assistant open
  writes one in the same transaction. The quote does become searchable and readable as part
  of the work request (`requestSQL`). That is expected and breaks nothing.

## Task 1: quote the user's words on a single hand-over

Implement decisions 1–7 in `internal/assistant/loop_effects.go`. Put the decision in one
small pure helper on the turn (effects and input in, which effect and what quote out), so
it is unit-testable without the store. Put the composition (label, blockquote, dedup,
command check) in another small pure function.

Tests go in `internal/assistant/loop_test.go`. Use `newLoopFixture`, `enqueue`, `act`,
`use` and `say`. Assert on `f.host.moves` and on `f.tapeTexts()`. An open shows up in
`f.host.moves` as `open <project>` followed by `send <id> <first instruction>`; a send
shows up as `send <id> <full delivered text>`. In every test the turn also adds a focus
item on the target, so the guard does not run extra rounds.

- **Quoted send:** user input «переделай на SQLite, схему не трогай»; the model sends to
  session 2 "Переведи локальный режим на SQLite". `f.host.moves` is exactly
  `send 2 Переведи локальный режим на SQLite\n\nThe user's own words:\n> переделай на SQLite, схему не трогай`.
  The tape's move line is still `→ devbox (2): "Переведи локальный режим на SQLite"`.
- **Quoted open:** user input «проверь redis в devbox, схему не трогай»; the model makes one
  open in devbox with text "Выясни, почему не стартует Redis". `f.host.moves` is exactly
  `open devbox` followed by `send 3 <composed string>` (the new session is id 3), `assistant_work.about` is unchanged, and the
  tape's move line shows only "Выясни, почему не стартует Redis".
- **Open + stop of an existing session:** no quote on the open.
- **Open + send to its handle:** no quote on either. The existing
  `TestAssistantLoopOpenHandleResolves` asserts exact moves and must keep passing unchanged.
- **Open + send to an existing session:** no quote on either.
  `TestAssistantLoopFailedOpenSkipsItsSends` and
  `TestAssistantLoopKeepsApplyingAfterABookkeepingError` expect `send 2 other` and must keep
  passing unchanged.
- **Two sends to two existing sessions:** no quote. Create the second addressable session
  with the existing helper `workSession(t, f.sessions, map[string]any{"manager_id": testOwner})`
  from `store_test.go`.
- **Two sends to the same existing session:** no quote on either.
- **Stop + send to the same session:** quoted. **Stop of another session + send:** not quoted.
- **Multi-line input:** each line gets a `> ` prefix; an empty line becomes `>`.
- **Whitespace and CRLF:** input "  a\r\nb\n" → quote is `> a\n> b`.
- **Command:** input «сожми контекст», a single send "/compact" to session 2 → the
  delivered text is exactly "/compact".
- **Non-user event:** a work-answer event whose turn sends to session 2 → no quote. Build
  the event input the way `TestAssistantLoopAnsweredSessionNeedsFocusUpdate` does.
- **Dedup:** input "go", send "go" → the delivered text is exactly "go". The existing
  `TestAssistantLoopGuardGivesUp` in `loop_test.go` and
  `TestAssistantLoopGuardFailureCommitsTheReply` in `loop_retry_test.go` keep passing
  unchanged.
- **Deliberate update:** `TestAssistantLoopGuardAsksForFocus` (input «redis лежит», send
  «почини redis») now expects the quoted form in `f.host.moves`. Update the expected value
  to the exact composed string; do not loosen the assertion to `Contains`.
Acceptance: everything above passes with `go test ./internal/assistant/ -run 'TestAssistant'`.
`TestHarnessScenario_AssistantShutdownAfterCommitKeepsEffects` and
`TestHarnessScenario_AssistantOpenAndRetell` each pass with their exact
`go test ./internal/daemon/ -run <Name>`.

## Task 2: tell the assistant

Add the sentence from decision 8 to `registry.AssistantPrompt`. Acceptance:
`go test ./internal/assistant/ -run TestAssistantToolsMatchPrompt` passes.

## Finish

Run `make all` once, bare, and read the output for `make: ***`. Then run `/pilat:arch-sync`
per `CLAUDE.md`. The assistant profile in `ARCHITECTURE.md` describes delivery; add a line
only if it says what a hand-over carries.
