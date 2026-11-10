# Glossary

The shared vocabulary of coagent — the words that name its concepts, so code, comments, and docs stay consistent and a reader never has to guess whether two names mean one thing. Each entry says what the term **is**; the `_Avoid_` line names the synonyms *not* to use for it. [ARCHITECTURE.md](../ARCHITECTURE.md) describes how these fit together — this file just names them.

**Keeping it alive:** add an entry the moment an ambiguity surfaces — someone asked what a term means, or two names showed up for one concept, or one name for two. Do it in that same turn, not in a cleanup pass at session end: by then the context that made the ambiguity obvious is gone. `/pilat:arch-sync`'s diff sweep is the backstop for whatever live sessions miss.

---

## Actors & entities

**daemon**:
The single long-lived coagent process. It coordinates session lifecycle, durable
producer ledgers and admission. `managercontrol`
implements the in-process `controllerapi.Controller` over the daemon's command
backend and reads persistence and subscriptions from their owners.
Domain packages own durable ledgers; the daemon owns runner lifecycle and
capacity counters. It binds no *network* socket — the only thing it
listens on is the **control socket**, a same-user unix socket.
_Avoid_: server, gateway.

**session**:
One task's isolated runtime — its own LLM client, tool registry, and conversation history — that runs the agent loop. "Session" names both the live object and the persisted `SessionRecord` row; the append-only design keeps those two deliberately distinct.
_Avoid_: conversation (that's the history), job.

**project confinement**:
The default, sandbox-enabled authority every session's processes and file tools
share, built from the **effective policy**: one ordered list of **sandbox
rule**s, evaluated last-match-wins, with **implicit default**s heading its
global and project sections. The project section ends with mandatory read-only
access to that project's process output, after its configured rules.
Applies to shell preparation, Bash, LSP, stdio MCP
processes and the `read`/`glob`/`grep`/`write`/`edit`/`apply_patch` tools alike;
`sandbox.enabled: false` is the explicit operator opt-out. It is an *integrity*
boundary, not confidentiality: with no rules, another checkout and the
operator's own credentials are readable, and only the project is writable. It
governs no network: sessions share the daemon's network namespace.
_Avoid_: sandbox (unqualified — implies more isolation than it gives), write
sandbox (it governs reads as well), allowlist/denylist (the policy is one
ordered list, not two sets), protected list, execution substrate, session
shields (all retired terms — see below).

**sandbox rule**:
One operator-authored entry in `sandbox.rules` or a project's `sandbox.projects.<path>.rules`:
exactly one of `allow` or `deny` naming an absolute or `~/`-prefixed path, with
`mode` (`ro` default, or `rw`) meaningful only on `allow`. Coagent ships no
credential or toolchain path lists; these rules name operator-owned paths. The
`projects:` map attaches rules to a project, it does not grant the project —
that comes from the **implicit default**.
_Avoid_: sandbox.denied, sandbox.writable_paths, writable_paths (retired
schema keys).

**implicit default**:
An unwritten rule at the head of the global section (`allow / ro`) or a
project's section (`allow <project root> rw`, plus the work directory and a
`/gwt` worktree's `.git` when applicable) that the operator's own rules are
written under. Because the project default sits after the global rules, a
broad global `deny` cannot make the project unwritable; only a rule inside that
project's own section can, and a `deny` naming the project root exactly is
honoured while a broader one that buries it incidentally is a configuration
error.
_Avoid_: built-in grant, base grant (both imply something coagent enumerates
rather than an unwritten rule position).

**effective policy**:
The compiled authority for one project: the ordered rule list with its
**implicit default**s resolved and mandatory read-only process output as the
last entry of the project section. Its **digest** identifies one policy
generation — authorized anchors (including declarations whose object is
absent), permissions and project identity — and never object presence or
inode. Compiled by `internal/sandboxpolicy`, which owns no process lifetime.
_Avoid_: "sandbox config" (that is the operator's input, not the compiled result).

**task**:
A unit of work a manager submits to the daemon. It has no Go type of its own — a task is realized as a **session** created from a prompt. Beware: the bare word `task` in code means only the **`task` tool** (which spawns subagents), never the work-unit — qualify accordingly.
_Avoid_: job, request (for the work-unit); unqualified "task" in code.

**Controller** (`controllerapi.Controller`):
The private manager-bound in-process capability `managercontrol` implements over
the daemon backend for built-in managers — create sessions, durably send
messages, stop/kill them, and subscribe to their owned session events. The
composition root binds one capability to each manager ID; session-addressed
operations reject every other owner. It is an internal package boundary, not a
supported external API.
_Avoid_: using "controller" for a front-end integration — that is a manager.

**manager**:
A built-in front end that drives the daemon through its private controller.
A Telegram manager owns one durable manager ID, one bot account, one private chat
with its sole allowed user and one polling instance. Enabled managers cannot
reuse a bot token. Adding a manager is a source-level contribution, not a
third-party plugin.
_Avoid_: controller, bot, adapter, integration.

**manager ownership**:
The durable routing identity of a root session: the unique manager ID stored as
`manager_id`. A manager subscribes with that same ID and receives only its own
session events. Its bound Controller also stamps new sessions and rejects reads
or mutations of any other owner; a driver or channel name is not ownership
because several managers may share it.
_Avoid_: channel ownership, transport ownership.

**subagent**:
An independent session with clean context and a restricted tool set, spawned by a parent session's `task` tool. A subagent *is* a session (its own `SessionRecord` row) whose `ParentID` identifies another session — that is what separates it from a root session. Subagents may suspend with `sleep`, but standalone scheduled work belongs only to roots.
_Avoid_: worker, child process (it is a session, not an OS process); using child or descendant when the entity rather than its graph direction is meant.

**subagent round**:
One bounded activation of an existing subagent session, ending in a completed, failed, or stopped outcome. Follow-ups accepted before terminalization join the active round in FIFO order; a follow-up accepted after terminalization starts the next serialized round, after the previous outcome is delivered. A subagent session may have many rounds while retaining the same conversation history and ID. A round is not a model-facing identity; persistence uses an internal monotonic `activation_seq` only to reject delayed completion signals from older rounds.
_Avoid_: run, execution (both are overloaded by the runner and agent loop); treating the subagent session itself as one disposable invocation.

**project**:
The identity a session runs in: one `projects` row keyed by absolute `work_dir`,
plus the folder's instructions, skills and curated memory. Any folder can be a
project, with or without code or Git; many independent sessions can share it.
_Avoid_: workspace, space; dialog (a dialog is a session in a project).

**agent type**:
A named agent configuration — tool allowlist, prompt template, model override, mode and optional project-context omission — that session assembly selects. Built-ins: `build`, `general`, `explore`, `browser`, `compaction`. The `task` tool offers only spawnable types; `build` and `compaction` are not subagent types. The built-in `explore` type is deliberately lean: it has read-only code-search tools and starts without project instructions, skills, curated memory, Git state or model catalog; its assignment must supply relevant constraints. The built-in `browser` type has only the tools of the MCP server bound by `browser.mcp_name` and the same project-context omission; without that binding it is unavailable. Browser disables integrated search. Agent type selects behavior and tools, never a work budget.
_Avoid_: role, persona, mode (mode is a separate axis on the same config).

**control socket**:
`~/.coagent/daemon.sock`, mode 0600 — newline-delimited JSON-RPC 2.0, the daemon's local API (`internal/ctl`). It carries the read-only `status` method and nothing else: no mutations, no chat, no server pushes. A unix socket is not a network listener; the "no inbound listener" rule is about the network.
The daemon begins accepting only after every startup op owner has registered, so a greeting is also a control-plane readiness boundary rather than a partially initialized view.
_Avoid_: RPC server, API server, IPC channel.

## Agent loop & context management

**context artifact**:
One of the three instruction files a session loads at open — **global**, **project**, or **local** — each resolved by a first-match-wins search over an ordered candidate list, then merged into the opening context as a `[role] <path>` labeled section with the home directory abbreviated to `~`. The section's path is the winning candidate, not the resolved symlink target.
_Avoid_: project instructions (the pre-rename name for the merged string), AGENTS.md (one candidate, not the resolved artifact).

**agent loop**:
The core cycle a session runs: accept boundary input, call the LLM, execute returned tools, and commit each step until done or capped. The entry point is `Run`.
_Avoid_: ReAct loop (doc-only alias), main loop.

**runner**:
The in-memory lifecycle holder for one active session: cancel/done boundary,
live session, working flag and admission metadata. Its registry,
launch, shutdown and terminal coordination belong to the daemon; it is
reconstructible and never a durable work record.
_Avoid_: session (the durable/live task identity), agent loop (the model/tool cycle).

**iteration**:
One turn of the agent loop — a single LLM call plus the tool executions it triggers. An observed count, not a limit: normal agent work has no iteration budget. The only cap is the internal 1000-iteration defect circuit breaker (`hardIterationCeiling`), which is not a tuning surface. An iteration is a *sub-unit* of the loop, not another word for it.
_Avoid_: using "iteration" and "agent loop" interchangeably; "iteration cap" or "budget" as if a normal terminal.

**compaction** (checkpoint):
The single automatic answer to context pressure: one no-tools model call summarizes a bounded older head — the native repaired conversation prefix replayed as ordinary messages plus one final checkpoint instruction — and the committed checkpoint rebuilds the transcript as header → marked summary → optional current-skill envelope → verbatim raw tail. The trigger is the token projection crossing 0.85 of the window, or image pressure breaching its high-water marks (12 MB base64 across attachments, or more than 20 of them). The complete summarizer request targets half the context window, falling back to the ordinary 85% input ceiling only when no legal candidate fits the target; the checkpoint retains a repair-free verbatim tail — at least a tenth of the window when that much history exists, possibly shorter under the tail's image byte/count ceilings (6 MB, 10), never empty. Repeated compaction replays the then-current prefix from the transcript beginning, prior marked summary included. Runs at exactly one point in the loop, where no tool call is pending ([ADR-0056](adr/0056-compaction-replays-the-native-bounded-prefix.md)).
_Avoid_: clearing (removed with ADR-0035); compression; "context ladder"; trim-before-summary (superseded); canonical JSONL input (superseded by ADR-0056); delta-only summarizer requests (superseded); image eviction (rejected — image bytes are answered by compaction, not by pruning history between compactions).

**checkpoint marker**:
The host-authored wrapper around model-authored summary text inside a compaction summary row. It identifies the text as a lossy summary of older history and says later verbatim messages are newer and take precedence on conflict. Only the complete wrapper at the one allowed position (immediately after the header) is recognized as a previous checkpoint.
_Avoid_: treating the wrapper as a security boundary — delimiter collisions in user content are accepted user-controlled content.

**deferral episode**:
The lifetime of the pending external call that made a `/compact` wait in the durable inbox. The "⏳ Compaction deferred" notice is deduplicated per episode — not per run or per wake — via a verdict the daemon carries across session rebuilds (`RunResult` → `deferAnnouncements` → the session assembly input).
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
Capping an oversized tool result *before* the tool step commits it to conversation history. What enters the transcript is already trimmed and never changes afterward, so the cached prompt prefix stays intact. The opposite — going back and editing messages already in the history (**retroactive pruning**) — invalidates the provider's prompt cache from the edited point onward, and coagent deliberately does not do it, except for replaced **browser frame**s.
_Avoid_: pruning (names the retroactive anti-pattern), clearing (a separate metadata event).

**loop detection**:
A diversity-based detector that catches repetitive tool-call patterns and forces the agent to break out. The "loop" here means *repetition* — unrelated to the **agent loop** (the execution cycle), despite the shared word.

**browser frame**:
The whole result of one call to the MCP server bound by `browser.mcp_name` inside a `browser` subagent, regardless of its content. Once a call to that server from a later model turn succeeds, the browser session's projection replaces the earlier frame with a fixed placeholder; failed calls never do, and the stored row never changes. The browser subagent's scoped exception to **insertion-time truncation**: only frames are replaced, never assistant messages or reasoning. Only the first call of an assistant message runs, so one turn yields at most one fresh frame, and a frame is truncated against a larger browser budget than other tool results ([ADR-0069](adr/0069-browser-takes-one-action-per-turn.md), amended by [ADR-0088](adr/0088-browser-surface-is-the-configured-mcp-server.md)).
_Avoid_: observation, screenshot (a frame may carry no image), image eviction (that rejected idea pruned all images, not superseded frames).

**browser task state**:
The model-authored `task_state` argument every call to the configured browser MCP server must carry: verdict on the previous action, task facts worth keeping from the current frame, next goal. It is the only record of a frame once that frame is replaced; coagent strips it before the call reaches the MCP server.
_Avoid_: memory (that is curated memory), state (a server tool may already have a `state` parameter), observation, page description (it records task progress, not what the screen looks like).

## Tools, skills & extensions

**tool**:
A capability the agent invokes — id, description, parameters, execute. Three origins: **built-in** (bash, read, edit, …), **MCP** (discovered from external servers), and **control-plane** (`task`, `schedule` — registered onto the live registry from outside and owned by the package that holds their state).

**untrusted tool output**:
Text returned by a web page, search provider, MCP server, or network-derived
command output that coagent treats as evidence rather than authority. Instructions
inside it cannot override system, user/project or task instructions. Host-added
markers are model-facing provenance hints, not a sandbox or formal security
boundary.
_Avoid_: treating external content as tool authority; **prompt injection** names
the attack, not the content category.

**parallel-safe tool**:
A tool whose Execute may run concurrently with its siblings in one assistant response, declared in code by `ParallelSafe() bool` (a compile-time contract, not an annotation — [ADR-0040](adr/0040-tool-calls-use-declared-ordered-scheduling.md)). The initial allowlist is `read`, `ls`, `glob`, `grep`, `webfetch`, `websearch`, `todoread`, `task`; MCP and every unlisted tool are serialized.
_Avoid_: thread-safe (an implementation property, not the declared scheduling policy), batch-able.

**tool trait**:
A property the core consults instead of a tool's ID, declared by the tool as an optional interface (`tool.UntrustedSource`, `tool.SkillLoader`, `tool.TrailDeclarer`, `tool.PromptDeclarer`) or, where stored rows must be classified after their tool is gone, by the tool stack as an ID rule (the **frame rule**) ([ADR-0083](adr/0083-core-recognises-tools-by-declared-traits.md), [ADR-0085](adr/0085-core-names-no-extensible-tool-by-literal.md)).
_Avoid_: tool kind.

**frame rule** (`tool.FrameRule`):
The tool stack's ID rule naming which tool calls return whole **browser frame**s and what truncation budget a frame gets. It answers for stored rows whose tool is no longer registered. An agent type opts in with `FrameSession`; such a **frame session** sees only the rule's tools, and every other type never sees them. Today the built-in browser surface declares the only rule and `browser` is the only frame session.
_Avoid_: browser check, Playwright check (the core never tests a tool ID against either name).

**MCP profile** (`mcp.Profile`):
The per-server adapter the tool stack hands to the MCP client for one server's tools — admit or refuse a discovered tool, extend its model-facing schema, gate a call's arguments before forwarding. `internal/mcp` applies it without knowing any server name; the browser surface's profile adds and strips **browser task state**.
_Avoid_: server hook, plugin.

**MCP server guidance**:
The server's initialization `instructions`, kept as metadata about using its tools.
The system prompt includes one delimited block per server with visible tools and
non-empty guidance, sorted by server name. The client caps each server's text at
8,000 Unicode characters, appending a marker when truncated. Guidance follows tool
visibility and never overrides system, user, project or task instructions; it is
distinct from **untrusted tool output** ([ADR-0089](adr/0089-mcp-instructions-follow-tool-visibility.md)).
_Avoid_: tool output, system authority.

**websearch**:
The builtin search tool (`internal/tool/builtin`), registered only when `tools.search` selects a REST provider — a model never sees a dead tool. Returns SERP-style results (title/url/snippet); page fetching stays `webfetch`'s job. With no `tools.search` section, `openrouter`-driver models instead get the driver's server-side search injection ([ADR-0043](adr/0043-integrated-search-quality-first.md)).
_Avoid_: web search tool vs MCP search servers (those keep their `mcp__…` tool IDs and coexist).

**tool stage**:
The unit of ordered scheduling (`internal/tool`): a maximal contiguous run of parallel-safe calls that executes concurrently through a four-slot rolling window; every other call is a singleton **barrier** stage that blocks later stages until it finishes. A failed, suspended, or cancelled call leaves later stages explicitly skipped (**fail-stop**), while its same-stage siblings still run. Result rows keep assistant call order regardless of completion order.
_Avoid_: worker pool (there is no shared queue), fan-out (the removed unconditional concurrency).

**skill**:
A `SKILL.md` instruction bundle loaded from project, global, or marketplace dirs. Two *independent* discovery axes: `disable-model-invocation: true` hides it from the model's available-skills inventory and skill tool; `user-invocable: false` rejects `/skill <name>`. A leading `/skill <name> [args]` expands before the LLM call.
The `task` tool may also seed a new subagent from one model-invocable skill: the parent resolves the canonical name and renders the envelope before spawn, so the child receives content rather than rediscovering the skill.
_Avoid_: plugin (a plugin is a marketplace bundle), command.

**marketplace**:
A git repo supplying loadable skills and subagent definitions, cloned and cached with a TTL. Configured under `marketplaces:` in `config.yaml`.

**MCP client**:
One external MCP-server process and its discovered tools, retained for one
session ID across loop activations. Each stack refreshes the live client's
catalog before use and keeps its schemas fixed during that activation. Policy,
configuration or shell-snapshot changes replace the client; stop/kill and daemon
shutdown close it. Distinct from the **MCP registry**, which stores server definitions.
_Avoid_: MCP pool, MCP catalog (neither has a separate lifetime).

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

**background Bash process**:
A finite shell command whose lifetime outlives one Bash tool call. It is owned by the exact session (root or subagent) that started it, capped at four advertised processes per owning session independently of the single foreground candidate, captured to one combined output file under `~/.coagent/processes/project-<project-id>/<session-id>/` (100 MiB per process), and killed with its deadline as a complete process group. Its terminal outcome (`completed`, `failed`, `timed_out`, `output_limit_exceeded`, `output_drain_timeout`, `cancelled`, `interrupted`) is a durable ledger row (`background_processes`, `internal/backgroundprocess`), never inferred from output text or a PID. See [ADR-0046](adr/0046-background-bash-process-lifecycle.md).
The starting session may explicitly cancel it using the advertised opaque `bgp_…` ID; this records `agent_cancelled`, releases the slot and suppresses completion input.
_Avoid_: daemon-owned process (the owner is the session, the daemon only administers), job.

**process completion input**:
A pending `source=process` row in the exact owning session's durable inbox, inserted atomically with an advertised background Bash process's terminal state. The loop promotes it as one bounded `<process_completion>` user-role turn carrying state, exit code, duration, output path and a 50-line/8-KiB preview; the output file stays the source for more. Explicit stop/kill cancellation creates no completion input, while a fact committed before stop remains pending.
It is model input only: it never enters manager output directly.
_Avoid_: process event (the removed synthetic tool pair), output passthrough.

**session state**:
The runtime status a controller sees — `running` / `idle` / `error` (`controllerapi.State*`), derived from the daemon's in-memory map and never persisted. Distinct from the persisted session **status** (`active` / `completed` / `suspended` / `stopping` / `stopped` / `error`) and from subagent **link state**.
_Avoid_: treating "running" (runtime) and "active" (persisted) as the same word.

**notification** (session event):
A session→controller event — a message chunk, a state change, a heartbeat (`sessionevent.Notification`, delivered as `controllerapi.SessionNotification`). Only *root* sessions have them: the daemon drops every event whose session is a subagent. Manager subscriptions additionally receive only events whose durable manager owner matches their ID. The bare type name "Notification" is overloaded elsewhere (LSP JSON-RPC, tool notifications), so qualify it as a *session event*.
_Avoid_: bare "Notification".

**model-input generation**:
The session-local monotonic counter that advances atomically when model-bound
input enters conversation history: inbox
promotion or standalone scheduled-turn injection. Pending input, tool results,
external-call completions, compaction and host-handled commands do not advance
it. Completion nudges use it to enforce one nudge per accepted input generation.
_Avoid_: iteration (loop counter), activation sequence (subagent delivery CAS).

**output log**:
The append-only `session_outbox` rows for owned root messages and lifecycle
facts, with stable source identities. It is neither the model transcript nor a
delivery queue; a per-chat **chat cursor** records the selected position.
_Avoid_: output queue, delivery backlog.

**chat focus**:
The durable root selected by one manager and chat, stored in
`chat_focus.session_id`. NULL means no session. Focus does not change which
sessions keep running. The manager binds its row at startup without overwriting
an existing selection; every selection write resets its **chat cursor**.
_Avoid_: active session (several sessions may run at once).

**chat cursor**:
`chat_focus.last_sent_id`, the selected session's highest output-log ID sent
or skipped past by one manager/chat. NULL means **entry** is owed. Numeric
positions advance only while the same focus remains entered. Selection writes,
including re-switch and clear, reset it to NULL; restart preserves it.
_Avoid_: per-session read tracking, transport receipt.

**entry**:
A selected session's bold `→ #N name` header followed by its last three renderable
output-log rows, oldest first, each with a **session header**. It repeats after
every selection write, including a re-switch. A compare-and-set then
acknowledges the newest ID of any type from the same read before live delivery.
_Avoid_: entry context.

**session title**:
A live owned root's one-line name, `sessions.attributes.title`, 1–80 characters.
The first inserted non-command user input names an untitled root once; the user
renames it with `/title`. A `/clear` replacement starts untitled. A chat shows
`<project> · <title>`. It is not `agent_type` and does not follow the work as it changes.
_Avoid_: label, session name.

**session header**:
The bold `#<id> <Name>` line, then a blank line, prefixed at delivery to every
renderable root row, including entry rows. It uses the `/sessions` display name
with asterisks removed and is never stored in the output log.
_Avoid_: entry header (the `→ #N <Name>` line that precedes entry rows).

**tool trail**:
The per-call lines of one manager-owned root's step message: `→ <tool>` or a
tool name with its main argument in inline code, redacted and clipped to 80
runes. Batch calls expand one level. The model's text precedes the trail when
present; a step with no text still publishes its trail. Subagents publish no
output-log rows.
_Avoid_: tool result, progress card, one message per call.

**tail**:
The selected session's output-log rows above that chat's **chat cursor**, read
in ascending ID order. Non-focused roots are never sent; selecting one enters its
last three renderable rows before starting its live tail.
_Avoid_: backlog, transcript.

**skill activation receipt**:
The output-log message that confirms one explicit skill
activation — `🔧 Activated skill: <canonical-name>`, nothing else. An explicit
`/skill` command commits it inside its promotion transaction (releasing, keyed
idempotently by the accepted input); the skill tool returns it as an ordinary
direct tool output. Only manager-owned roots publish it; system-injected
skills and auto-activation produce none.
_Avoid_: activation notice, skill notification.

**autonomous episode**:
The interval of manager-owned root work starting with initial or reactivating
model-bound input, or an applied scheduled turn. Queued input keeps the current
episode; empty roots and read-only commands create none. Its durable start
anchors the wall time shown by `/status`.
_Avoid_: accepted task, queued message, run (ambiguous with process activation).

**one-shot budget**:
A root-tree checkpoint over additional persisted USD cost, additional wall
time, or both, set under the user's `/budget` grant. One generation is armed
from durable baselines, fires once, closes admission and parks the tree, then is
released by the next model-bound **human input** to the root. Control and
read-only commands still pass. It is not a permanent spending policy.
_Avoid_: iteration budget, billing limit, permanent ceiling.

**human input**:
Every session input with `source = 'user'`. Model-bound human input to a root
can release a fired budget; a matching leading command creates its exact
durable tool activation grant.
_Avoid_: real user message (authority follows the source field).

**tool activation grant**:
Durable one-turn authority created only by a manager-owned **human input** whose
leading command matches a tool's declared activation command. It is bound to
the exact input/session/tool/command and is consumed only by the first valid
matching mutation; prompt text is never authority.
_Avoid_: command text authorization, hidden tool.

**suspend** (`ErrSuspend`):
A sentinel error `sleep` returns to checkpoint and exit the agent loop *without* recording a result; the timer's exact result is injected on resume. The persisted status is `suspended`. Standalone `schedule` creates future work but does not suspend the calling session.

**session input**:
A user/agent action, scheduled turn, exact call result or process/subagent completion addressed to an
existing session and accepted into the durable `session_inbox` FIFO before any
consumer observes it. A normal message or bounded completion envelope is
promoted into the append-only transcript; a generic session command is
handled from the same ledger without entering the LLM. Manager-specific UI
actions such as project spawning, pickers and masked secret prompts remain with
the manager. The same input path handles a live, idle, suspended, or stopped
session; “steering” and “waking” are no longer separate data transports.
_Avoid_: steering message, wake message, in-memory inbox.

**waiting projection**:
A structured `NotifyWaiting` session event projected from authoritative ledgers: pending sleep rows that own an exact `metadata.tool_call_id`, plus foreground subagent links. One-shot schedules without `tool_call_id` are future inputs, not waits. Managers render that projection; they never infer “sleeping” from a generic suspension or transcript text. Multiple foreground children currently form an all-wait set.
_Avoid_: synthetic sleep notice, polling message.

**pending sleep**:
A one-shot schedule carrying `metadata.tool_call_id`, and therefore owning the eventual result of one exact suspended `sleep` call. `PendingSleeps` is the only schedule-domain projection used for waiting UI and interruption. A one-shot without `tool_call_id` is standalone scheduled input and must survive `/stop` or interruption of a different sleep.
_Avoid_: one-shot wait (ambiguous), any one-shot is sleep.

**schedule occurrence**:
One due one-shot or sleep, or one cron minute. An applied occurrence's producer
acknowledgement commits with its session input: the row is deleted or its last-fired time
advances. An acknowledged retry returns `applied=false`. Cron uses one canonical
minute for admission and payload.
_Avoid_: scheduled delivery identity (the retired delivery-key contract).

**scheduled root**:
A root with at least one schedule that is not a pending sleep. The `⏰` marker
appears in `/sessions` and in its entry header; individual delivered rows do not
repeat it.

**scheduled turn**:
New standalone work injected by a one-shot or cron schedule, rather than the
result of a pending external call. Only a root session may own a scheduled turn.
A due scheduled turn may reactivate a stopped root in the transaction that
acknowledges its producer and enqueues its input; an acknowledged retry leaves
that root stopped. A legacy scheduled occurrence addressed to a subagent is
acknowledged as an unapplied no-op.
_Avoid_: sleep wake, subagent wake.

**admission control**:
The daemon's concurrency governor — caps on total, child, and per-parent
sessions plus spawn depth, decided in the runner set together with
durable-aware FIFO overflow and runner registration.

**subagent link ledger** (`subagent.Store`):
The subagent package's durable record (`subagent_links` table) of parent↔child
relationships, serialized activation state (`activation_seq`), and
parent-delivery state. A stopped link preserves the child session for explicit
follow-up without making startup resume it.

**pending external call**:
A tool call whose outcome comes from outside the loop — a sleep timer, a subagent or a config apply across a restart. The loop never re-executes one and never advances past it; transcript repair never stubs one. Exact call-result inbox rows, producer ledgers and the config-apply marker establish ownership; only that producer, stop or the boot orphan sweep resolves it.
_Avoid_: suspended call, blocked tool.

**interrupted in-loop call**:
A non-external tool call left pending in the transcript by a daemon restart. The boot sweep settles it as a typed failure before the session resumes, so the loop never re-executes an operation the model did not watch complete; the model retries explicitly. See [ADR-0059](adr/0059-pending-tool-calls-are-never-reexecuted-on-resume.md).
_Avoid_: orphaned call (the orphaned-call pass covers external calls only), re-executed call.

**stop boundary**:
The durable `/stop` transition for an active root tree. It fences producers,
cancels and joins runners, settles active unresolved calls, then marks the tree
stopped. The daemon owns the tree fence, transcript and activation settlement,
then producer cancellation and the terminal manager output. Later root
input and explicit child follow-up are new work; neither replays pre-stop calls.
_Avoid_: cancellation alone, pause.

**pending apply**:
A config change that has been written and is waiting for the restart to confirm it. Its marker (`~/.coagent/pending-apply.json`) carries the session to answer, the backup to restore, and the hash of the config that was about to land — which is what lets the next boot tell "the write never happened" from "the write landed and something else broke".
_Avoid_: transaction, staged config (that is the pre-write `Staged`).

## Memory & persistence

**memory**:
The *curated* per-project long-term store (`CuratedStore` over the `memories`
table), edited via `memory_save` / `memory_delete`. A best-effort inventory is
persisted with project instructions in a fresh session's marked opening row and
remains frozen for that session; later opening turns read the updated store. It
is not the conversation transcript.
_Avoid_: history, context (those are the transcript, not memory).

**conversation history** (message store):
The append-only durable history of a session's message rows. The agent loop
derives the **provider transcript** from it; durable rows excluded by projection
remain history without becoming model input. Distinct from **memory**.
_Avoid_: memory, provider transcript (that is a projection of this history).

**provider transcript**:
The ordered message projection submitted to a model call. It is derived from
conversation history and may omit durable rows that are not valid model input.
_Avoid_: conversation history (that also includes rows outside the projection).

**model attempt**:
One completed provider call and its returned response, including normalized and
provider-native finish reasons, content, reasoning, tool calls and usage.
Acceptance into the provider transcript is a separate session decision; a
rejected attempt remains durable evidence.
_Avoid_: iteration (one loop iteration may include other durable transitions),
assistant message (only an accepted attempt has that transcript role).

**model finish reason**:
The provider-reported reason a model attempt stopped generating, preserved in
native form and normalized as `llmwire.FinishType` for response-integrity
routing. `stop` / `end_turn` means the response ended normally; it does not by
itself prove that the requested task is complete. Whether an accepted response
ends an activation is a separate agent-loop decision.
_Avoid_: task completion, final answer, terminal signal.

**background wake source**:
A durable process or subagent obligation that will deliver model-bound input
after the current activation ends, including a completion already queued in the
session inbox. It is established from producer ledgers and inbox state, not from
prompt text or a process-local snapshot, and therefore survives daemon restart.
_Avoid_: unfinished todo, pending external call, background prompt snapshot.

**completion check**:
The session-wide second-look transition for a non-empty `stop` response with no
tool calls and no background wake source. The first response is a final
candidate, hidden from delivery, and a
host-authored model input asks the same agent to continue or confirm stopping.
The confirmation publishes the candidate's text, not the confirming response;
a durable pointer to the candidate row lets a subagent's parent receive the
same answer. A later external model-bound input or tool-bearing response
invalidates the check; an empty response remains subject to loop detection.
At most one nudge is issued per model-input generation: once the model continued
with tools after it, its next text stop is final and published as-is
([ADR-0068](adr/0068-one-completion-nudge-per-model-input.md)).
_Avoid_: stop marker, magic acknowledgement, final-answer tool.

**background-exit nudge**:
The host-authored model input a subagent receives when it stops while its own
advertised background processes still run. It lists them and offers two exits:
cancel unneeded ones and answer, or end the turn and be resumed on completion;
either way the final answer replaces everything said before. It shares the completion check's one-nudge-per-generation marker.
A subagent's yield with live processes never becomes its parent's result
([ADR-0071](adr/0071-subagent-result-waits-for-its-own-background-work.md)).
_Avoid_: interim result, waiting marker.

**attachment** (referenced image attachment):
A disk reference stored on a tool-result row in `messages.attachments` — never
the pixels themselves. Alongside path, MIME, and size, a `read` persists its
canonical read root so later materialization cannot follow a replacement path
outside the original authority. Produced by `read` on a supported image;
drivers re-materialize it into content blocks on every request, gated
fail-closed on the catalog's input modalities, degrading to an inline
placeholder when unmaterializable. Telegram uploads produce metadata text only;
seeing pixels always takes a `read` roundtrip
([ADR-0034](adr/0034-vision-via-referenced-tool-result-attachments.md)).
_Avoid_: "image message", inline base64, upload-attached media.

## Configuration & isolation

**coagent home**:
`~/.coagent` — the daemon's own state directory: config, secrets, control socket, DB, caches and `projects/`. Resolved exclusively by `internal/coagenthome`, which also names everything inside it. Tests isolate resolution under `t.TempDir()` through `HOME`; test binaries reject the inherited user home and its descendants. Distinct from a task repo's project-level `.coagent/` context dir (`config.ProjectCoagentDir`) — same directory name, different scope.
_Avoid_: "config dir" for the whole directory; conflating it with the project-level `.coagent/`.

**LSP server**:
A user- or project-owned language-server executable discovered through the
project's activated shell PATH. Coagent neither downloads, installs, nor pins
it.
_Avoid_: managed LSP installation, PATH fallback.

**provider**:
A user-named entry in `config.yaml`'s `providers:` map — credentials, `base_url`, which **driver** to use, and an optional `catalog` key. It is the operator's label for one endpoint, not a vendor.
_Avoid_: using "provider" for the driver, or for the vendor behind an OpenAI-compatible endpoint.

**driver**:
The private `llm.driverProtocol` implementation that owns one provider protocol end to end: constructing clients (`NewClient`) and answering where its models' characteristics come from (`ListModels`). Four exist: `anthropic`, `openai`, `openrouter`, `google-sa`. Adding one means implementing both halves — the compiler will not let a new driver skip its catalog.

**catalog** (models):
The external source of model metadata — display name, context window, max output tokens, pricing, reasoning capability. models.dev for the `anthropic` / `openai` / `google-sa` drivers (section selected by the provider's `catalog` key), OpenRouter's own `/api/v1/models` for `openrouter`. Fetched once at daemon startup, cached under `~/.coagent/cache/catalog`, and the **only** source for those fields — config carries none of them, and a model it does not know is a startup error ([ADR-0003](adr/0003-external-model-catalogs.md)) unless that model has a **last resolution**.
_Avoid_: "model config" for this data.

**last resolution**:
What a model's catalog last answered for it, kept per (model, origin) pair in `~/.coagent/cache/catalog/resolved-models.json`. It is the catalog's own past answer, not an override or a second source, and losing the file reinstates the startup error. A model its catalog no longer lists runs on its last resolution and shows as `(stale catalog)` in `/model` and `coagent status` ([ADR-0086](adr/0086-resolved-model-survives-catalog-drop.md)).
_Avoid_: "previous catalog", "cached body" (those are the whole catalog snapshots the fetcher keeps).

**model tag**:
A user-defined lowercase ASCII token on a configured model. Tags are hints with
no built-in routing meaning: they bound which configured models `task` may offer
for autonomous explicit selection, while `/model` remains available for every
configured model.
_Avoid_: capability, model role.

**secrets**:
The in-memory credential map parsed from `~/.coagent/secrets`, deliberately kept out of the process environment (tool subprocesses inherit no credentials) and scrubbed from all log output.

**shellenv**:
A per-session, per-cwd snapshot of a login+interactive shell (mise / asdf / nvm / direnv toolchain activation), captured lazily and replayed for Bash / LSP / MCP subprocess spawns. It survives loop activations under the same policy; fingerprint validation and a 30-minute backstop keep it fresh ([ADR-0001](adr/0001-shellenv-fingerprint-invalidation.md)). Resource retirement removes its snapshots. Capture and `type -P` lookup execute through the session's confinement runner. The snapshot is exposed to the sandbox read-only through an inherited descriptor — never the user cache directory. Captures `os.Environ()` only — never a secrets map.

**filesystem-write sandbox**:
Retired as a term: the default is **project confinement**, one ordered
allow/deny rule list for reads and writes alike. `sandbox.enabled: false`
remains the operator's explicit opt-out and restores unrestricted host reads
with no write confinement.
_Avoid_: using it for the default boundary.

**composition root**:
`cmd/coagent/main.go` — hand-wires every component in dependency order (no DI framework) and records a named stop closure per component, replayed in reverse on shutdown.

**service install**:
The one install layout ([ADR-0009](adr/0009-system-daemon-user-binary.md)): a
*system* systemd unit (`/etc/systemd/system`) that drops the daemon to the
login user, pointing at a binary in `~/.local/bin/coagent`. Root writes the
unit once; the binary stays user-owned so updates need none.
_Avoid_: install scope, user install, `--user` (there is no second mode — "scope" belongs to the MCP registry).

**escalation gate**:
`runDaemonVerb` — the single place that decides a `coagent <verb>` (install/uninstall/start/stop/restart) needs root and re-execs itself under `sudo`. Everything below it is privilege-blind.
_Avoid_: sudo wrapper, privilege helper (nothing here is a persistent helper process).

---

## Flagged ambiguities

Naming conflicts this vocabulary resolves. "Resolved" means the winner above is the term to use going forward; where existing code or prose still uses a loser, fix it on touch rather than in a sweep.

- **controller vs manager** — *Resolved.* `controllerapi.Controller` is the private manager-bound interface implemented by `managercontrol` over the daemon backend; a built-in front end is a **manager**. It does not name a public extension point.
- **task (work-unit) vs `task` tool** — *Resolved.* "task" is prose for a submitted unit of work, realized as a session with no `Task` type; the only code symbol named `task` is the subagent-spawning tool. Qualify in code contexts.
- **state / status (three vocabularies)** — *Resolved.* Runtime **state** = `controllerapi.State*` (running/idle/error, in-memory); persisted **status** = active/completed/suspended/error; subagent **link state** = `subagent.State*`. Don't treat "running" and "active" as one word.
- **agent loop vs ReAct loop vs iteration** — *Resolved.* Canonical is **agent loop** (entered through `Run`); "ReAct loop" is an acceptable doc alias; **iteration** is one turn, not a synonym for the loop.
- **clearing vs compaction** — *Resolved.* **Compaction** is the whole operation and the only automatic pressure response; clearing (dropping tool-result bodies) was removed with [ADR-0035](adr/0035-compaction-summarizes-a-bounded-head.md) — summarizer evidence is serialized, never erased. The old "context ladder" is retired.
- **Notification (overloaded type)** — *Resolved.* Use **session event** / `sessionevent.Notification` for live session→controller events; it collides with `lsp.Notification` (JSON-RPC). Durable session input is a `sessionstore.Input` inbox row.
- **memory vs conversation history** — *Resolved.* "memory" means the curated `CuratedStore` only; the transcript is **conversation history** / the message store.
- **loop (agent) vs loop (detection)** — *Resolved.* The **agent loop** is the execution cycle; **loop detection** is about repetitive-call *repetition*. Same word, unrelated meanings — always qualify.
- **project vs space/workspace/dialog (folders for non-code chats)** — *Resolved.* A folder for notes/blog-style dialogs is an ordinary **project** (daemon-provisioned ones default under `~/.coagent/projects/`); no separate container term. "workspace" stays unused (git connotation); a dialog is a session in a project, never the folder itself.
