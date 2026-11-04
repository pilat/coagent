# ADR-0080: The assistant administers work at `open` and hears the user's direct exchanges

- **Status:** Superseded by [ADR-0082](0082-manager-is-a-groupless-chat-without-an-assistant.md)
- **Date:** 2026-10-30
- **Amends:** [ADR-0040](0040-gwt-forks-a-worktree-off-the-remote-default-branch.md),
  [ADR-0074](0074-assistant-is-an-ordinary-session-with-a-desk-head.md),
  [ADR-0075](0075-work-answers-reach-the-observer-through-its-inbox.md),
  [ADR-0077](0077-assistant-moves-the-user-in-never-out.md),
  [ADR-0078](0078-assistant-input-is-never-human-authority.md)
- **Amended by:** [ADR-0081](0081-assistant-forwards-and-the-host-relays-work-answers.md) — observed answers are relayed into `#0`; the assistant speaks on them only when it adds something

## Context

The owner's complaints about the assistant as administrator of work sessions came down to
four gaps.

- **The first turn ran on the wrong model.** `open` creates the root on the default model
  and enqueues the instruction immediately. A `set_model` that follows (ADR-0077) arrives
  after the first turn has started. In session 777 the default model wrote "this session
  has no Claude Opus", and Opus then repeated it from the transcript. The session's only
  self-knowledge was `- Model: <raw id>`, appended at the end of the prompt and missing
  for some agent types.
- **The assistant could not see or schedule.** Cards had neither model nor budget. Its
  `schedule` tool reached only its own root.
- **Direct exchanges were invisible.** ADR-0075 notifies the assistant only about
  instructions it sent: "a final that answers only the user's direct messages produces
  nothing". The obvious fix, marking the user's input with `observer_session_id`, runs
  into ADR-0078. `IsHumanInput` treats the presence of that key as assistant provenance,
  so the owner's own `/budget`, `/skill` and budget lift would silently lose authority.
- **No worktree.** In session 26379 the assistant told a work session to create a git
  worktree inside a dirty checkout. The session cannot change its own directory; project
  and sandbox are bound to `work_dir`. The `/gwt` code lives in `managercontrol`, which
  `daemon` may not import.

Three facts constrained the design:
- ADR-0074 has no silent turns, so every event the assistant receives costs a line in
  `#0`.
- The daemon does not know the chat focus. Typed text, voice transcripts, attachments,
  session commands and numbered `/kill N` all reach a work root on one path.
- `/gwt` runs host-side git, outside the sandbox, in a repository whose `.git` the work
  session can write. ADR-0040 deliberately suppresses no hooks, and git also executes
  commands from repository config: `core.fsmonitor`, filter drivers, credential helpers,
  `core.sshCommand`, `uploadpack`.

## Decision

- **`open(project, about, text, model?, effort?, worktree?)`.** Model and effort are
  validated like `set_model`'s before anything is created, and written into the root at
  creation, before the first enqueue. Requesting a model for new work means passing it to
  `open`.
- **Every session's prompt states its model.** The line is `You are running on <display
  name> (<id>), effort <level>.`, plus a sentence that this line outranks earlier claims
  in the conversation. The build path and `SwitchModel` render it identically, for every
  agent type. The build agent's identity rule narrows: who it is stays "Coagent", but a
  direct question about the running model is answered from this line. The line and cards
  render the effective effort using the driver's catalog clamp, rather than an unsupported
  stored level; an empty stored level uses the client's `medium` fallback before clamping.
- **Cards show model, effort, budget and `⏰`.** Remaining budget is computed once, on
  `budget.Record`, and shared with `/status`.
- **Human input is `source = 'user'` without `from = assistant`.** `observer_session_id`
  no longer decides authority. `from = assistant` has one writer, `instructTx`, which
  always writes the observer reference too, and no controller path accepts inbox-row
  attributes.
- **Human input to an owned work root is observed.** In the enqueue transaction,
  `sessionstore` stamps the manager's assistant root as `observer_session_id` on every
  human input to a non-assistant root. The seven commands that owe no model reply are
  excluded (`isExactControlCommand`). The input stays human: its grants and budget lift
  are unchanged.
- **The quote rides inside the event.** An `answered` or `failed` event starts with one
  `→ <project> (<id>) user wrote: "…"` line per human ref row in its window, and one
  `⏰ <project> (<id>) schedule #N fired` line per reported schedule input. There is no
  separate quote event and no extra wake. The prompt asks for one short line on
  exchanges the user had directly.
- **The assistant schedules work roots.** Its `schedule` tool takes `session`, `cron` or
  `at`, `prompt`, `fresh` and `report`. It lists its own and its work roots' schedules
  and cancels by id, never a sleep row. `report` stores the assistant root in the
  schedule's metadata. The executor then puts it on the fired input as the observer
  reference, so the run's answer arrives as an ordinary `answered` event. Without
  `report`, a run is the work root's own business.
- **`⏰` marks a root with a non-sleep schedule.** It appears in `/sessions`, the switch
  header and cards, using one sleep predicate.
- **`open` with `worktree` runs the `/gwt` code** from a new `worktree` package that both
  `managercontrol` and `daemon` call. It runs in a **no-execution mode**:
  - every git command of the sequence gets a fixed `-c` override set that disables
    hooks, `fsmonitor`, `ext::` and submodule recursion;
  - a preflight refuses any `include.path` or `includeIf.*.path` in local or worktree
    scope, without evaluating conditions or following the includes. A conditional include
    inactive in the source checkout can activate in the new worktree and introduce a
    command-running key. The refusal points to `/gwt`; global includes remain allowed;
  - the same preflight refuses local or worktree keys that run a command and cannot be
    overridden without breaking the user's own global setup: filter drivers, credential
    helpers, `core.sshCommand`, `core.gitProxy`, `core.alternateRefsCommand`,
    `core.askPass`, `remote.*.uploadpack`.

  Human `/gwt` keeps ADR-0040's behavior and runs the repository's hooks.

## Consequences

- Session 777's failure cannot recur on `open`. A model asked about itself has an
  authoritative line, and the line survives model switches and restarts.
- An effort-only `set_model` rewrites the cached system block once.
- The assistant learns of a direct exchange only when the work session answers it, or
  fails. A user's own stop or kill reports nothing (ADR-0075 silent lifecycle). Each
  exchange costs one assistant activation and one short line in `#0`. If that proves
  noisy, the next step is to accumulate exchanges until the user returns to `#0`.
- Sessions the user only talked to directly now count as touched. They appear in the
  desk's recent sessions and show "awaiting answer" while the user's message is open.
  This is intended.
- An input with an `observer_session_id` but no `from` (the old test shape) is now human.
  Every test that pinned the old rule changes.
- A tick final of an unreported schedule is still reported if it lands while another ref
  row of the root is open, because the window joins every non-yield final.
- Assistant-created worktrees skip the repository's setup hooks. A repository whose own
  config defines an include directive, filters, a credential helper or similar keys needs
  a human `/gwt`. The user's global helpers, ssh wrappers and LFS filters keep working.
- The override set and the preflight list are a **denylist**. A git key that executes a
  command and is named in neither bypasses the mode.
- The preflight needs git 2.26 or newer (`--show-scope`). On an older git, `open` with
  `worktree` is refused.
- The mode does not touch the root cause, which is that the sandbox lets work sessions
  write `.git/config`, `.git/hooks` and `.git/info`. A planted config also runs when the
  user runs ordinary git in a terminal. Denying those writes for work sessions is
  separate follow-up work.

## Alternatives Considered

- **Keep `set_model` after `open`, or tell the prompt to wait.** Rejected: the race is
  structural; the first enqueue wakes the runner.
- **Keep `IsHumanInput` and add a second attribute for "observed human input".**
  Rejected: two keys with nearly the same meaning in the hottest commit path, and a
  window query over both.
- **A separate quote event when the user writes.** Rejected: with no silent turns, every
  direct message would cost two activations and two lines in `#0`.
- **A quote row that does not wake the assistant.** Rejected: it is the chosen design
  reached through a second mechanism, plus a new non-waking input class in the wake path.
- **Never report scheduled runs.** Rejected: "check every morning and tell me" is the
  common request, and without `report` its answer would sit unread in a work session.
- **A second scheduling tool for work sessions.** Rejected: one tool with an optional
  target mirrors `set_budget`.
- **Move `/gwt` into `daemon` behind `Backend`.** Rejected: git plumbing would settle
  into the orchestrator. A small package keeps the tiers honest.
- **Accept the hook risk as `/gwt`'s.** Rejected by the owner. A human triggers `/gwt`.
  Here a work session can plant a hook and then nudge an LLM through its answer into
  running it on the host.
- **Disable hooks only (`core.hooksPath=/dev/null`).** Rejected: `fsmonitor`, filters
  and credential helpers execute just as well.
- **Override every command-running key, credential helpers and `sshCommand`
  included.** Rejected: the override also drops the user's global keychain helper and
  ssh wrapper, so fetch from every private remote would fail. Refusing only
  repository-scope definitions keeps the user's setup.
- **Run the git sequence inside the session sandbox.** It would close the class rather
  than a list. Rejected for this change: a new sandboxed launch path for daemon-side git
  carries more work and risk than the feature. The root-cause fix belongs in the sandbox
  policy instead.
