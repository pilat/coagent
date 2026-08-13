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
The private in-process interface the daemon implements for built-in managers — create sessions, durably send messages, stop/kill them, and subscribe to session events. It is an internal package boundary, not a supported external API.
_Avoid_: using "controller" for a front-end integration — that is a manager.

**manager**:
A built-in front end (Telegram or local chat today) that drives the daemon through `controllerapi.Controller`. A new manager is a source-level contribution, not a third-party plugin.
_Avoid_: controller, bot, adapter, integration.

**subagent**:
An independent child session with clean context, a restricted tool set, and its own iteration budget, spawned by a parent session's `task` tool. A subagent *is* a session (its own `SessionRecord` row) — that is what separates it from a top-level task.
_Avoid_: worker, child process (it is a session, not an OS process).

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
The reserved folder-project (`ProjectName = "coagent"`) the CLI chat's session lives in. An ordinary project in every respect; the name is what makes it findable.
_Avoid_: admin project, system project.

**bootstrap**:
The deterministic half of a first run, before any model is involved: install-or-update the daemon, then collect one provider and its key over the control socket. Everything past that is the chat.
_Avoid_: wizard, setup flow (for the chat half).

**onboarding skill**:
The setup guide embedded in the binary (`internal/loader/builtin/onboarding`), registered only on CLI-channel root sessions. Its script calls `request_secret`, which no other channel has.
_Avoid_: setup agent, onboarding agent (it is not an agent type).

## Agent loop & context management

**agent loop**:
The core cycle a session runs: call the LLM, execute the returned tool calls, record the observations, repeat until done or capped. The function is `runLoop`.
_Avoid_: ReAct loop (doc-only alias), main loop.

**iteration**:
One turn of the agent loop — a single LLM call plus the tool executions it triggers. Bounded by a max-iterations cap. An iteration is a *sub-unit* of the loop, not another word for it.
_Avoid_: using "iteration" and "agent loop" interchangeably.

**clearing** (tool-result clearing):
Replacing older `role='tool'` result bodies with a uniform placeholder while keeping the tool call itself visible (re-run the tool to recover). Not a context event of its own: it is compaction's first phase, preparing the feed for the summarizer ([ADR-0013](adr/0013-immutable-history-single-compaction-point.md)).
_Avoid_: compaction, pruning, truncation; calling it a stage or rung of its own.

**compaction**:
The single automatic answer to context pressure: clear older tool bodies, LLM-summarize everything after the header, and rebuild the transcript as header → summary turn → skill reattachments. No verbatim tail survives; the summary turn carries the model's brief plus a programmatically capped excerpt of the last turns and the still-running background work. Runs at exactly one point in the loop, where no tool call is pending.
_Avoid_: clearing; compression; "context ladder" (there is no ladder — the term is retired).

**deferral episode**:
The lifetime of the pending external call that made a `/compact` wait in the durable inbox. The "⏳ Compaction deferred" notice is deduplicated per episode — not per run or per wake — via a verdict the daemon carries across session rebuilds (`RunResult` → `deferAnnouncements` → `CreateOptions`).
_Avoid_: per-activation notice (the retired behavior).

**compaction projection**:
The number the compaction trigger and `/status` both read: the provider's last reported cache-inclusive `PromptTokens` plus a `len/4` estimate of everything appended since that measurement. With no measurement (fresh session, resume, subagent, right after a compaction, after a model switch) it is a plain whole-transcript estimate, and `/status` marks it with a tilde.
_Avoid_: calibration (the removed scale-factor machinery), token budget.

**output reserve**:
The share of a model's context window left for the response — the complement of the compaction threshold (`1 − llmwire.ContextInputFraction`). The OpenAI-compatible client clamps request `max_tokens` to it, so input and output budgets compose without counting input tokens ([ADR-0010](adr/0010-output-budget-clamps-max-tokens.md)).
_Avoid_: max_tokens (as a name for the budget rather than the wire field), output budget.

**append-only context log**:
The invariant that stored message content is immutable after insert. Clearing and compaction are metadata events (`cleared_at` / `compacted_at`) plus appended rows; "what the model sees" is a projection computed at load, so the prompt prefix stays byte-stable between compactions — nothing edits history in between.

**insertion-time truncation**:
Capping an oversized tool result *before* it is appended to the conversation history (`toolexec.go`). What enters the transcript is already trimmed and never changes afterward, so the cached prompt prefix stays intact. The opposite — going back and editing messages already in the history (**retroactive pruning**) — invalidates the provider's prompt cache from the edited point onward, and coagent deliberately does not do it.
_Avoid_: pruning (names the retroactive anti-pattern), clearing (a separate metadata event).

**loop detection**:
A diversity-based detector that catches repetitive tool-call patterns and forces the agent to break out. The "loop" here means *repetition* — unrelated to the **agent loop** (the execution cycle), despite the shared word.

## Tools, skills & extensions

**tool**:
A capability the agent invokes — id, description, parameters, execute. Three origins: **built-in** (bash, read, edit, …), **MCP** (discovered from external servers), and **control-plane** (`task`, `schedule`, `compact_context` — registered onto the live registry from outside and owned by the package that holds their state).

**skill**:
A `SKILL.md` instruction bundle loaded from project, global, or marketplace dirs. Two *independent* visibility axes: `disable-model-invocation: true` hides it from the model's prompt/tool inventory; `user-invocable: false` rejects `/skill <name>`. A leading `/skill <name> [args]` expands before the LLM call.
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
A session→controller event — a message chunk, a state change, a heartbeat (`sessionevent.Notification`, delivered as `controllerapi.SessionNotification`). Only *root* sessions have them: `svc.publish` drops every event whose session is a subagent, so a controller never sees one. The bare type name "Notification" is overloaded elsewhere (LSP JSON-RPC, tool notifications), so qualify it as a *session event*.
_Avoid_: bare "Notification".

**suspend** (`ErrSuspend`):
A sentinel error a tool (`sleep`, `schedule`) returns to checkpoint and exit the agent loop *without* recording a result; the real result is injected on resume. The persisted status is `suspended`.

**session input**:
A normal user or agent message accepted into the durable `session_inbox` FIFO before any runner observes it. The session promotes it into the append-only transcript at the next agent-loop boundary. The same path handles a live, idle, suspended, or stopped session; “steering” and “waking” are no longer separate data transports.
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

**admission control**:
The daemon's concurrency governor — caps on total, child, and per-parent sessions plus spawn depth, with a FIFO overflow queue.

**subagent link ledger** (`LinkStore`):
The daemon's durable record (`subagent_links` table) of parent↔child subagent relationships, serialized activation state (`activation_seq`), and parent-delivery state. A stopped link preserves the child session for explicit follow-up without making startup resume it.

**pending external call**:
A tool call whose outcome comes from outside the loop — a sleep timer, a subagent, a config apply across a restart, a person typing at a terminal. The loop never re-executes one and never advances past it; transcript repair never stubs one; only an injection targeting its call id resolves it. The daemon's in-memory **staged-call ledger** records the ones it is itself answering.
_Avoid_: suspended call, blocked tool.

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

## Configuration & isolation

**coagent home**:
`~/.coagent` — the daemon's own state directory: config, secrets, control socket, DB, caches, `projects/`, `bin/`. Resolved exclusively by `internal/coagenthome`, which also names everything inside it. Tests isolate resolution under `t.TempDir()` through `HOME` or `coagenthome.Override`; test binaries reject the inherited user home and its descendants. Distinct from a task repo's project-level `.coagent/` context dir (`config.ProjectCoagentDir`) — same directory name, different scope.
_Avoid_: "config dir" for the whole directory; conflating it with the project-level `.coagent/`.

**provider**:
A user-named entry in `config.yaml`'s `providers:` map — credentials, `base_url`, which **driver** to use, and an optional `catalog` key. It is the operator's label for one endpoint, not a vendor.
_Avoid_: using "provider" for the driver, or for the vendor behind an OpenAI-compatible endpoint.

**driver**:
The private `llm.driverProtocol` implementation that owns one provider protocol end to end: constructing clients (`NewClient`) and answering where its models' characteristics come from (`ListModels`). Four exist: `anthropic`, `openai`, `openrouter`, `google-sa`. Adding one means implementing both halves — the compiler will not let a new driver skip its catalog.

**catalog**:
The external source of model metadata — display name, context window, max output tokens, pricing, reasoning capability. models.dev for the `anthropic` / `openai` / `google-sa` drivers (section selected by the provider's `catalog` key), OpenRouter's own `/api/v1/models` for `openrouter`. Fetched once at daemon startup, cached under `~/.coagent/cache/catalog`, and the **only** source for those fields — config carries none of them, and a model the catalog does not know is a startup error ([ADR-0003](adr/0003-external-model-catalogs.md)).
_Avoid_: "model config" for this data.

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
- **clearing vs compaction** — *Resolved.* **Compaction** is the whole operation and the only automatic pressure response; **clearing** is its first phase (drop tool-result bodies), never an event on its own. The old "context ladder" is retired.
- **Notification (overloaded type)** — *Resolved.* Use **session event** / `sessionevent.Notification` for session→controller events; it collides with `lsp.Notification` (JSON-RPC). Daemon transcript delivery is now a sealed typed `sessionInput`, not another notification bag.
- **memory vs conversation history** — *Resolved.* "memory" means the curated `CuratedStore` only; the transcript is **conversation history** / the message store.
- **loop (agent) vs loop (detection)** — *Resolved.* The **agent loop** is the execution cycle; **loop detection** is about repetitive-call *repetition*. Same word, unrelated meanings — always qualify.
- **project vs space/workspace/dialog (folders for non-code chats)** — *Resolved.* A folder for notes/blog-style dialogs is an ordinary **project** (daemon-provisioned ones default under `~/.coagent/projects/`); no separate container term. "workspace" stays unused (git connotation); topic/thread name the Telegram dialog *on* a project, never the folder.
