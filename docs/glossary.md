# Glossary

The shared vocabulary of coagent — the words that name its concepts, so code, comments, and docs stay consistent and a reader never has to guess whether two names mean one thing. Each entry says what the term **is**; the `_Avoid_` line names the synonyms *not* to use for it. [ARCHITECTURE.md](../ARCHITECTURE.md) describes how these fit together — this file just names them.

**Keeping it alive:** add an entry the moment an ambiguity surfaces — someone asked what a term means, or two names showed up for one concept, or one name for two. Do it in that same turn, not in a cleanup pass at session end: by then the context that made the ambiguity obvious is gone. `/pilat:arch-sync`'s diff sweep is the backstop for whatever live sessions miss.

---

## Actors & entities

**daemon**:
The single long-lived coagent process. It owns session lifecycle, persistence, the MCP pool, admission control, and the subagent ledger, and it *implements* the in-process `controllerapi.Controller`. It binds no *network* socket — the only thing it listens on is the **control socket**, a same-user unix socket.
_Avoid_: server, gateway.

**session**:
One task's isolated runtime — its own LLM client, tool registry, and conversation history — that runs the agent loop. "Session" names both the live object and the persisted `SessionRecord` row; the append-only design keeps those two deliberately distinct.
_Avoid_: conversation (that's the history), job.

**task**:
A unit of work a manager submits to the daemon. It has no Go type of its own — a task is realized as a **session** created from a prompt. Beware: the bare word `task` in code means only the **`task` tool** (which spawns subagents), never the work-unit — qualify accordingly.
_Avoid_: job, request (for the work-unit); unqualified "task" in code.

**Controller** (`controllerapi.Controller`):
The private manager-bound in-process capability the daemon implements for built-in managers — create sessions, durably send messages, stop/kill them, and subscribe to their owned session events. The composition root binds one capability to each manager ID; session-addressed operations reject every other owner. It is an internal package boundary, not a supported external API.
_Avoid_: using "controller" for a front-end integration — that is a manager.

**manager**:
A built-in front end (Telegram or local chat today) that drives the daemon through `controllerapi.Controller`. A new manager is a source-level contribution, not a third-party plugin.
_Avoid_: controller, bot, adapter, integration.

**manager ownership**:
The durable routing identity of a root session: the unique manager ID stored as
`manager_id`. A manager subscribes with that same ID and receives only its own
session events. Its bound Controller also stamps new sessions and rejects reads
or mutations of any other owner; a driver or channel name is not ownership
because several managers may share it.
_Avoid_: channel ownership, transport ownership.

**subagent**:
An independent session with clean context, a restricted tool set, and its own iteration budget, spawned by a parent session's `task` tool. A subagent *is* a session (its own `SessionRecord` row) whose `ParentID` identifies another session — that is what separates it from a root session. Subagents may suspend with `sleep`, but standalone scheduled work belongs only to roots.
_Avoid_: worker, child process (it is a session, not an OS process); using child or descendant when the entity rather than its graph direction is meant.

**subagent round**:
One bounded activation of an existing subagent session, ending in a completed, failed, or stopped outcome. Follow-ups accepted before terminalization join the active round in FIFO order; a follow-up accepted after terminalization starts the next serialized round, after the previous outcome is delivered. A subagent session may have many rounds while retaining the same conversation history and ID. A round is not a model-facing identity; persistence uses an internal monotonic `activation_seq` only to reject delayed completion signals from older rounds.
_Avoid_: run, execution (both are overloaded by the runner and agent loop); treating the subagent session itself as one disposable invocation.

**project**:
The identity a session runs in: one `projects` row keyed by absolute `work_dir` (`GetOrCreateProject`), plus whatever the folder itself carries — `CLAUDE.md`/`AGENTS.md`, skills, curated memory. Nothing requires code or git: any folder is a project, and one project can host many Telegram-topic dialogs (notes, blog drafts, a repo — same machinery).
_Avoid_: workspace, space; dialog (a dialog is a topic *on* a project, not the project).

**agent type**:
A named agent configuration — tool allowlist, prompt template, iteration cap, mode — that the `task` tool selects from. Built-ins: `build`, `general`, `explore`, `compaction`. Not every agent type is spawnable (`build`/`compaction` are not subagent types).
_Avoid_: role, persona, mode (mode is a separate axis on the same config).

**control socket**:
`~/.coagent/daemon.sock`, mode 0600 — newline-delimited JSON-RPC 2.0, the daemon's local API (`internal/ctl`). It carries `status`, the bootstrap config ops, `restart_daemon`, and the CLI chat, in both directions: responses and server pushes share one connection. A unix socket is not a network listener; the "no inbound listener" rule is about the network.
The daemon begins accepting only after every startup op owner has registered, so a greeting is also a control-plane readiness boundary rather than a partially initialized view.
_Avoid_: RPC server, API server, IPC channel.

**CLI manager** (`internal/managers/cli`):
The built-in local chat — a peer of the Telegram manager that drives the same `controllerapi.Controller` over the control socket. It is not a config entry: it exists whenever the daemon runs, because it is how a daemon with no config gets one.
_Avoid_: TUI, configurator, console UI.

**coagent project**:
The reserved system project (`sys:coagent`) that owns the CLI configuration chat. Its identity is the logical name together with the canonical `<projects_root>/sys_coagent` path: user project names cannot contain `:`, user project creation cannot claim that directory, and an internal marker cannot grant authority to another path.
_Avoid_: admin project, ordinary CLI project.

**bootstrap**:
The deterministic half of a first run, before any model is involved: install-or-update the daemon, then collect one provider and its key over the control socket. Everything past that is the chat.
_Avoid_: wizard, setup flow (for the chat half).

**onboarding skill**:
The setup protocol embedded in the binary (`internal/loader/builtin/onboarding`) and automatically active only in the coagent project's terminal root session. Its script calls `request_secret`, which requires both that project identity and the CLI channel.
_Avoid_: setup agent, onboarding agent (it is not an agent type).

## Agent loop & context management

**agent loop**:
The core cycle a session runs: call the LLM, execute the returned tool calls, record the observations, repeat until done or capped. The function is `runLoop`.
_Avoid_: ReAct loop (doc-only alias), main loop.

**iteration**:
One turn of the agent loop — a single LLM call plus the tool executions it triggers. Bounded by a max-iterations cap. An iteration is a *sub-unit* of the loop, not another word for it.
_Avoid_: using "iteration" and "agent loop" interchangeably.

**compaction** (checkpoint):
The single automatic answer to context pressure: one no-tools model call summarizes a bounded older head — the repaired canonical JSONL projection — and the committed checkpoint rebuilds the transcript as header → marked summary → optional current-skill envelope → verbatim raw tail. The trigger is the token projection crossing 0.85 of the window, or image pressure breaching its high-water marks (12 MB base64 across attachments, or more than 20 of them). The complete summarizer request stays within half the context window; the checkpoint retains a repair-free verbatim tail — at least a tenth of the window when that much history exists, possibly shorter under the tail's image byte/count ceilings (6 MB, 10), never empty. Runs at exactly one point in the loop, where no tool call is pending ([ADR-0035](adr/0035-compaction-summarizes-a-bounded-head.md)).
_Avoid_: clearing (removed with ADR-0035); compression; "context ladder"; trim-before-summary (superseded); image eviction (rejected — image bytes are answered by compaction, not by pruning history between compactions).

**checkpoint marker**:
The host-authored wrapper around model-authored summary text inside a compaction summary row. It identifies the text as a lossy summary of older history and says later verbatim messages are newer and take precedence on conflict. Only the complete wrapper at the one allowed position (immediately after the header) is recognized as a previous checkpoint.
_Avoid_: treating the wrapper as a security boundary — delimiter collisions in user content are accepted user-controlled content.

**deferral episode**:
The lifetime of the pending external call that made a `/compact` wait in the durable inbox. The "⏳ Compaction deferred" notice is deduplicated per episode — not per run or per wake — via a verdict the daemon carries across session rebuilds (`RunResult` → `deferAnnouncements` → `CreateOptions`).
_Avoid_: per-activation notice (the retired behavior).

**compaction projection**:
The number the compaction trigger and `/status` both read: the provider's last reported cache-inclusive `PromptTokens` plus a `len/4` estimate of everything appended since that measurement. The measurement persists across restarts and is discarded when the model changed; with no measurement (fresh session, right after a compaction, after a model switch) it is a plain whole-transcript estimate, and `/status` marks it with a tilde.
_Avoid_: calibration (the removed scale-factor machinery), token budget.

**output reserve**:
The share of a model's context window left for the response — the complement of the compaction threshold (`1 − llmwire.ContextInputFraction`). The OpenAI-compatible client clamps request `max_tokens` to it, so input and output budgets compose without counting input tokens ([ADR-0010](adr/0010-output-budget-clamps-max-tokens.md)).
_Avoid_: max_tokens (as a name for the budget rather than the wire field), output budget.

**append-only context log**:
The invariant that stored message content is immutable after insert. Compaction is a metadata event (`compacted_at`) plus appended rows; "what the model sees" is a projection computed at load, so the prompt prefix stays byte-stable between compactions — nothing edits history in between.

**insertion-time truncation**:
Capping an oversized tool result *before* it is appended to the conversation history (`toolexec.go`). What enters the transcript is already trimmed and never changes afterward, so the cached prompt prefix stays intact. The opposite — going back and editing messages already in the history (**retroactive pruning**) — invalidates the provider's prompt cache from the edited point onward, and coagent deliberately does not do it.
_Avoid_: pruning (names the retroactive anti-pattern), clearing (a separate metadata event).

**loop detection**:
A diversity-based detector that catches repetitive tool-call patterns and forces the agent to break out. The "loop" here means *repetition* — unrelated to the **agent loop** (the execution cycle), despite the shared word.

## Tools, skills & extensions

**tool**:
A capability the agent invokes — id, description, parameters, execute. Three origins: **built-in** (bash, read, edit, …), **MCP** (discovered from external servers), and **control-plane** (`task`, `schedule` — registered onto the live registry from outside and owned by the package that holds their state).

**skill**:
A `SKILL.md` instruction bundle loaded from project, global, or marketplace dirs. Two *independent* discovery axes: `disable-model-invocation: true` hides it from the model's available-skills inventory and skill tool; `user-invocable: false` rejects `/skill <name>`. A leading `/skill <name> [args]` expands before the LLM call. Daemon-selected system instructions, currently the onboarding skill, may be activated directly without becoming model-invocable.
_Avoid_: plugin (a plugin is a marketplace bundle), command.

**marketplace**:
A git repo supplying loadable skills and subagent definitions, cloned and cached with a TTL. Configured under `marketplaces:` in `config.yaml`.

**MCP pool**:
The daemon-level pool of external MCP-server *connections*, keyed by a hash of command+args+env+workdir, refcounted, and reaped after 30 minutes idle — so servers aren't re-spawned per task. Distinct from the **MCP registry**, which says which servers exist at all.

**MCP registry**:
The DB-backed set of MCP server *definitions* (`mcp_servers` table, `internal/mcpstore`), managed conversationally with `mcp_add` / `mcp_remove` / `mcp_enable` / `mcp_disable` / `mcp_list`. Rows carry an `enabled` flag; env values hold `${VAR}` references literally and are resolved at acquire time. Changes reach a session at its next run, never mid-run.
_Avoid_: "MCP config" (there is no longer a `mcp:` YAML section).

**reasoning effort**:
The per-session level a reasoning-capable model runs at, drawn from the gateway vocabulary `none` / `minimal` / `low` / `medium` / `high` / `xhigh` / `max`. **Which of these a given model accepts is a catalog fact, not a constant** — every model carries its own allowlist (`ModelEntry.EffortLevels`, weakest first) and its own default (`ModelEntry.DefaultEffort`), and a model switch resets the session to that model's default. Each driver renders the level differently: the native Anthropic driver sends adaptive thinking plus an effort level, or a `budget_tokens` fraction of the model's output limit for older models; OpenRouter sends its unified `reasoning: {effort}`. A model whose catalog declares no effort selector, or one on a driver that never sends the level, has an empty allowlist and gets no picker step and no level on the wire.
_Avoid_: "thinking budget" (that is one driver's encoding of the level, not the level); "the three levels" (the set is per-model).

**effort allowlist**:
The levels one model accepts, as its catalog declares them — OpenRouter's per-model `reasoning.supported_efforts`, models.dev's `reasoning_options[].values`. Normalized to canonical weakest-first order by `catalog.SortEfforts`; a requested level outside it is clamped to the nearest by `catalog.ClampEffort` rather than sent raw.

**scope** (MCP registry):
Which sessions a registry row applies to: `global` (`project_id IS NULL` — every project) or `project` (one project). A project row overrides a global one of the same name, including when it is disabled.

## State, notifications & lifecycle

**session state**:
The runtime status a controller sees — `running` / `idle` / `error` (`controllerapi.State*`), derived from the daemon's in-memory map and never persisted. Distinct from the persisted session **status** (`active` / `completed` / `suspended` / `stopping` / `stopped` / `error`) and from subagent **link state**.
_Avoid_: treating "running" (runtime) and "active" (persisted) as the same word.

**notification** (session event):
A session→controller event — a message chunk, a state change, a heartbeat (`sessionevent.Notification`, delivered as `controllerapi.SessionNotification`). Only *root* sessions have them: `svc.publish` drops every event whose session is a subagent. Manager subscriptions additionally receive only events whose durable manager owner matches their ID; daemon-internal observers may inspect all root events. The bare type name "Notification" is overloaded elsewhere (LSP JSON-RPC, tool notifications), so qualify it as a *session event*.
_Avoid_: bare "Notification".

**model-input generation**:
The session-local monotonic counter (plus its transcript boundary message ID)
that advances atomically only when model-bound input enters conversation
history — ordinary inbox promotion and standalone scheduled-turn injection.
Pending inbox input, tool results, external-call completions, compaction, and
host-handled commands never advance it. Every manager message output snapshots
the current generation as host-owned outbox metadata in its insertion
transaction; progress narration is scoped to rows after the boundary.
_Avoid_: iteration (loop counter), activation sequence (subagent delivery CAS).

**replaceable output**:
A durable manager-output message that may reuse the external message identities
of the immediately preceding replaceable output only when both adjacent rows
carry the same model-input generation — or both carry none under the legacy
rule. A changed or mixed generation starts a new external message. A following
same-generation persistent output may reuse those identities once and closes
the replaceable chain. Managers whose transport cannot edit append a new
rendering instead.
_Avoid_: manager replacement, cursor.

**persistent output**:
A durable manager-output message that cannot be replaced by later output. It
closes an immediately preceding replaceable chain, potentially by editing that
chain's external messages; otherwise it creates new external messages.
_Avoid_: final output (persistent output is not necessarily a task result).

**direct reply**:
The first non-empty assistant text produced for newly promoted manager input
when that same response also calls tools. It is persistent but non-releasing,
so the human's message keeps an adjacent immutable answer while subsequent
progress uses a new replaceable output. A published direct reply is excluded
from progress narration rather than repeated inside the card.
_Avoid_: final output, progress note.

**autonomous episode**:
The interval of manager-owned root work that starts with initial or reactivating
model-bound input, or an applied scheduled turn. Queued input keeps the current
episode; empty roots and read-only commands do not create one. Its durable start
anchors operator wall time and the five-minute silence deadline.
_Avoid_: accepted task, queued message, run (ambiguous with one process activation).

**progress snapshot**:
The canonical root-session operator view: runtime/persisted state, live context
projection when available, lifetime persisted usage, autonomous-episode wall
time, durable TODO items, latest model progress, exact waits, subagent topology
and an optional one-shot budget. Automatic cards, `/status`, reconnect and final
footers render this shared transport-neutral value.
_Avoid_: progress message (the output remains replaceable output), heartbeat.

**one-shot budget**:
A user-authorized root-tree checkpoint over additional persisted USD cost,
additional wall time, or both. One generation is armed from durable baselines,
fires once, closes admission and parks the tree, then is released by the next
ordinary model-bound root input. It is not a permanent spending policy.
_Avoid_: iteration budget, billing limit, permanent ceiling.

**tool activation grant**:
Durable one-turn authority created only by a manager-owned user input whose
leading command matches a tool's declared activation command. It is bound to
the exact input/session/tool/command and is consumed only by the first valid
matching mutation; prompt text is never authority.
_Avoid_: command text authorization, hidden tool.

**manager delivery receipt**:
The manager-owned external message identities recorded when one durable output
is acknowledged. It is scoped by manager ID, may contain multiple identities
for a chunked rendering, and is stored and returned without daemon-side
interpretation.
_Avoid_: message ID (singular), cursor.

**suspend** (`ErrSuspend`):
A sentinel error `sleep` returns to checkpoint and exit the agent loop *without* recording a result; the timer's exact result is injected on resume. The persisted status is `suspended`. Standalone `schedule` creates future work but does not suspend the calling session.

**session input**:
A user or agent action addressed to an existing session and accepted into the
durable `session_inbox` FIFO before any consumer observes it. A normal message
is promoted into the append-only transcript; a generic session command is
handled from the same ledger without entering the LLM. Manager-specific UI
actions such as project spawning, pickers and masked secret prompts remain with
the manager. The same input path handles a live, idle, suspended, or stopped
session; “steering” and “waking” are no longer separate data transports.
_Avoid_: steering message, wake message, in-memory inbox.

**waiting projection**:
A structured `NotifyWaiting` session event projected from authoritative ledgers: pending sleep rows that own an exact `metadata.tool_call_id`, plus foreground subagent links. Metadata-free one-shot schedules are future inputs, not waits. Managers render that projection; they never infer “sleeping” from a generic suspension or transcript text. Multiple foreground children currently form an all-wait set.
_Avoid_: synthetic sleep notice, polling message.

**pending sleep**:
A one-shot schedule carrying `metadata.tool_call_id`, and therefore owning the eventual result of one exact suspended `sleep` call. `PendingSleeps` is the only schedule-domain projection used for waiting UI and interruption. A metadata-free one-shot is standalone scheduled input and must survive `/stop` or interruption of a different sleep.
_Avoid_: one-shot wait (ambiguous), any one-shot is sleep.

**scheduled delivery identity**:
A deterministic ID for one standalone one-shot or cron occurrence. Its fingerprint and transcript mutation commit together in `session_deliveries`; an identical producer retry is accepted with `applied=false`, while different semantics under the same ID fail closed. Cron uses one canonical minute for ID and payload; fresh delivery fingerprints stable prompt context, not stamped wall-clock text.
_Avoid_: schedule message ID, retry token.

**scheduled turn**:
New standalone work injected by a one-shot or cron schedule, rather than the result of a pending external call. Only a root session may own a scheduled turn. A due scheduled turn may reactivate a stopped root after its deterministic delivery identity is claimed; duplicate acknowledgement leaves that root stopped. A legacy scheduled occurrence addressed to a subagent is acknowledged as an unapplied no-op.
_Avoid_: sleep wake, subagent wake.

**admission control**:
The daemon's concurrency governor — caps on total, child, and per-parent sessions plus spawn depth, with a FIFO overflow queue.

**subagent link ledger** (`LinkStore`):
The daemon's durable record (`subagent_links` table) of parent↔child subagent relationships, serialized activation state (`activation_seq`), and parent-delivery state. A stopped link preserves the child session for explicit follow-up without making startup resume it.

**pending external call**:
A tool call whose outcome comes from outside the loop — a sleep timer, a subagent, a config apply across a restart, a person typing at a terminal. The loop never re-executes one and never advances past it; transcript repair never stubs one; only an injection targeting its call id resolves it. The daemon's in-memory **staged-call ledger** records the ones it is itself answering.
_Avoid_: suspended call, blocked tool.

**stop boundary**:
The durable `/stop` transition for an active root tree. It fences producers,
cancels and joins runners, settles active unresolved calls, then marks the tree
stopped. Later root input and explicit child follow-up are new work; neither
replays pre-stop calls.
_Avoid_: cancellation alone, pause.

**pending apply**:
A config change that has been written and is waiting for the restart to confirm it. Its marker (`~/.coagent/pending-apply.json`) carries the session to answer, the backup to restore, and the hash of the config that was about to land — which is what lets the next boot tell "the write never happened" from "the write landed and something else broke".
_Avoid_: transaction, staged config (that is the pre-write `Staged`).

## Memory & persistence

**memory**:
The *curated* per-project long-term store (`CuratedStore` over the `memories` table), surfaced in the system prompt and edited via `memory_save` / `memory_delete`. It is not the conversation transcript.
_Avoid_: history, context (those are the transcript, not memory).

**conversation history** (message store):
The append-only transcript of a session's messages — what the agent loop reads and writes each turn. Distinct from **memory**.
_Avoid_: memory.

**attachment** (referenced image attachment):
A disk reference (`{path, mime, size}`) stored on a tool-result row in `messages.attachments` — never the pixels themselves. Produced by `read` on a supported image; drivers re-materialize it into content blocks on every request, gated fail-closed on the catalog's input modalities, degrading to an inline placeholder when unmaterializable. Telegram uploads produce metadata text only; seeing pixels always takes a `read` roundtrip ([ADR-0034](adr/0034-vision-via-referenced-tool-result-attachments.md)).
_Avoid_: "image message", inline base64, upload-attached media.

## Configuration & isolation

**coagent home**:
`~/.coagent` — the daemon's own state directory: config, secrets, control socket, DB, caches and `projects/`. Resolved exclusively by `internal/coagenthome`, which also names everything inside it. Tests isolate resolution under `t.TempDir()` through `HOME` or `coagenthome.Override`; test binaries reject the inherited user home and its descendants. Distinct from a task repo's project-level `.coagent/` context dir (`config.ProjectCoagentDir`) — same directory name, different scope.
_Avoid_: "config dir" for the whole directory; conflating it with the project-level `.coagent/`.

**LSP server**:
A user- or project-owned language-server executable discovered through the
project's activated shell PATH. Coagent neither downloads, installs, nor pins it.
_Avoid_: managed LSP installation, PATH fallback.

**provider**:
A user-named entry in `config.yaml`'s `providers:` map — credentials, `base_url`, which **driver** to use, and an optional `catalog` key. It is the operator's label for one endpoint, not a vendor.
_Avoid_: using "provider" for the driver, or for the vendor behind an OpenAI-compatible endpoint.

**driver**:
The private `llm.driverProtocol` implementation that owns one provider protocol end to end: constructing clients (`NewClient`) and answering where its models' characteristics come from (`ListModels`). Four exist: `anthropic`, `openai`, `openrouter`, `google-sa`. Adding one means implementing both halves — the compiler will not let a new driver skip its catalog.

**catalog**:
The external source of model metadata — display name, context window, max output tokens, pricing, reasoning capability. models.dev for the `anthropic` / `openai` / `google-sa` drivers (section selected by the provider's `catalog` key), OpenRouter's own `/api/v1/models` for `openrouter`. Fetched once at daemon startup, cached under `~/.coagent/cache/catalog`, and the **only** source for those fields — config carries none of them, and a model the catalog does not know is a startup error ([ADR-0003](adr/0003-external-model-catalogs.md)).
_Avoid_: "model config" for this data.

**model tag**:
A user-defined lowercase ASCII token on a configured model. Tags are hints with
no built-in routing meaning: they bound which configured models `task` may offer
for autonomous explicit selection, while `/model` remains available for every
configured model.
_Avoid_: capability, model role.

**secrets**:
The in-memory credential map parsed from `~/.coagent/secrets`, deliberately kept out of the process environment (tool subprocesses inherit no credentials) and scrubbed from all log output.

**shellenv**:
A per-cwd snapshot of a login+interactive shell (mise / asdf / nvm / direnv toolchain activation), captured, cached (validated by a fingerprint of the on-disk toolchain state, with a 30-min backstop — see [ADR-0001](adr/0001-shellenv-fingerprint-invalidation.md)), and replayed for bash / LSP / MCP subprocess spawns. Captures `os.Environ()` only — never a secrets map.

**filesystem-write sandbox**:
Optional native write-confinement for Bash descendants and the `write` / `edit` / `apply_patch` tools (Seatbelt on macOS, Bubblewrap on Linux). It is an *integrity* boundary — not confidentiality: it does not confine reads or network egress.
_Avoid_: sandbox (unqualified — implies more isolation than it gives).

**composition root**:
`cmd/coagent/main.go` — hand-wires every component in dependency order (no DI framework) and records a named stop closure per component, replayed in reverse on shutdown.

**service install**:
The one install layout per platform ([ADR-0009](adr/0009-system-daemon-user-binary.md)): a *system* unit/plist (systemd `/etc/systemd/system`, launchd `/Library/LaunchDaemons`) that drops the daemon to the login user, pointing at a binary in `~/.local/bin/coagent`. Root writes the unit once; the binary stays user-owned so updates need none.
_Avoid_: install scope, user install, `--user` (there is no second mode — "scope" belongs to the MCP registry).

**escalation gate**:
`runDaemonVerb` — the single place that decides a `coagent daemon <verb>` needs root and re-execs itself under `sudo`. Everything below it is privilege-blind.
_Avoid_: sudo wrapper, privilege helper (nothing here is a persistent helper process).

**unit drift**:
The installed unit/plist no longer matching what the running version would render. Binary updates never rewrite it, so the update path renders-and-compares and *warns*; it never escalates to fix drift on its own.

---

## Flagged ambiguities

Naming conflicts this vocabulary resolves. "Resolved" means the winner above is the term to use going forward; where existing code or prose still uses a loser, fix it on touch rather than in a sweep.

- **controller vs manager** — *Resolved.* `controllerapi.Controller` is the daemon-implemented private interface; a built-in front end is a **manager**. It does not name a public extension point.
- **task (work-unit) vs `task` tool** — *Resolved.* "task" is prose for a submitted unit of work, realized as a session with no `Task` type; the only code symbol named `task` is the subagent-spawning tool. Qualify in code contexts.
- **state / status (three vocabularies)** — *Resolved.* Runtime **state** = `controllerapi.State*` (running/idle/error, in-memory); persisted **status** = active/completed/suspended/error; subagent **link state** = `LinkState*`. Don't treat "running" and "active" as one word.
- **agent loop vs ReAct loop vs iteration** — *Resolved.* Canonical is **agent loop** (matches `runLoop`); "ReAct loop" is an acceptable doc alias; **iteration** is one turn, not a synonym for the loop.
- **clearing vs compaction** — *Resolved.* **Compaction** is the whole operation and the only automatic pressure response; clearing (dropping tool-result bodies) was removed with [ADR-0035](adr/0035-compaction-summarizes-a-bounded-head.md) — summarizer evidence is serialized, never erased. The old "context ladder" is retired.
- **Notification (overloaded type)** — *Resolved.* Use **session event** / `sessionevent.Notification` for session→controller events; it collides with `lsp.Notification` (JSON-RPC). Daemon transcript delivery is now a sealed typed `sessionInput`, not another notification bag.
- **memory vs conversation history** — *Resolved.* "memory" means the curated `CuratedStore` only; the transcript is **conversation history** / the message store.
- **loop (agent) vs loop (detection)** — *Resolved.* The **agent loop** is the execution cycle; **loop detection** is about repetitive-call *repetition*. Same word, unrelated meanings — always qualify.
- **project vs space/workspace/dialog (folders for non-code chats)** — *Resolved.* A folder for notes/blog-style dialogs is an ordinary **project** (daemon-provisioned ones default under `~/.coagent/projects/`); no separate container term. "workspace" stays unused (git connotation); topic/thread name the Telegram dialog *on* a project, never the folder.
- **Telegram group forum vs bot forum** — *Resolved.* A **group forum** is a
  topic-enabled supergroup containing the bot; a **bot forum** is the topic-enabled
  private chat between one user and the bot. Both contain Telegram **topics**,
  but neither topology is itself a topic.
- **Telegram manager vs Telegram bot** — *Resolved.* A configured Telegram
  **manager** has one durable manager ID, one forum target and one Telegram bot
  account. Each manager owns its polling instance; two enabled managers must not
  reuse the same bot token.
