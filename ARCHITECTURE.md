# coagent — Architecture

How coagent's packages interact, what they own, and the rules that govern their boundaries. This is the single architecture document for the repository: system-wide topology and rules come first, then per-package internals in [Package Internals](#package-internals).

> **Scope**: This document covers the Go codebase (`cmd/`, `internal/`). Telegram support is implemented as an embedded Go manager runtime (`internal/managers/telegram`).

## Contents

System-wide: [Rules for new code](#rules-for-new-code) · [System overview](#system-overview) · [Package dependency graph](#package-dependency-graph) · [Package ownership](#package-ownership) · [Interface boundaries](#interface-boundaries) · [Agent types and delegation](#agent-types-and-delegation) · [Cross-cutting decisions](#cross-cutting-architectural-decisions) · [Data lifecycle](#data-lifecycle) · [Error propagation](#error-propagation-strategy) · [Initialization](#initialization-sequence) · [Shutdown](#shutdown-sequence) · [Cross-package contracts](#cross-package-contracts) · [Keeping this document accurate](#keeping-this-document-accurate)

Per-package internals (in [Package Internals](#package-internals)):

| Package | What it owns |
|---------|--------------|
| [`internal/daemon`](#pkg-daemon) | Session lifecycle, persistence, PubSub, admission, subagent ledger (`LinkStore`) |
| [`internal/session`](#pkg-session) | Per-task ReAct loop, history, compaction, loop detection, tool orchestration, delegation |
| [`internal/llm`](#pkg-llm) | Provider drivers (client construction + `ListModels`), catalog enrichment, retry, cost tracking |
| [`internal/catalog`](#pkg-catalog) | Catalog transport: HTTP fetch, disk cache, id matching and the effort vocabulary — names no endpoint, parses no format |
| [`internal/tool`](#pkg-tool) | Tool/Result/Registry contract, tool-ID constants, truncation budget — pure protocol leaf, no implementations |
| [`internal/bashsandbox`](#pkg-bashsandbox) | Native filesystem-write confinement runner (Seatbelt/Bubblewrap) |
| [`internal/shellenv`](#pkg-shellenv) | Per-cwd login+interactive shell snapshot (toolchain activation) for tool subprocesses |
| [`internal/tool/builtin`](#pkg-builtin) | Built-in tool implementations (bash, read, write, edit, glob, grep, ls, lsp, memory, batch, ...) + `BuildStack` (session tool-stack wiring) |
| [`internal/mcp`](#pkg-mcp) | MCP server connections, pooling, eviction, tool discovery, `AcquireForWorkDir` |
| [`internal/mcpstore`](#pkg-mcpstore) | MCP server registry (`mcp_servers` table): global/project-scoped definitions |
| [`internal/memory`](#pkg-memory) | Curated per-project memory (plain CRUD over the `memories` table) |
| [`internal/registry`](#pkg-registry) | Agent-type taxonomy, built-in types, prompt templates, per-session `Set` |
| [`internal/schedule`](#pkg-schedule) | Schedule storage, cron validation, one-shot/recurring scheduling, executor, schedule/sleep tools |
| [`internal/loader`](#pkg-loader) | CLAUDE.md parsing, SKILL.md loading, subagent definitions, marketplace git cloning |
| [`internal/todo`](#pkg-todo) | In-memory task list for the agent |
| [`internal/lsp`](#pkg-lsp) | LSP server lifecycle, code-intelligence queries |
| [`internal/managers`](#pkg-managers) | Manager runtime: starts/stops configured controller-side integrations (e.g. Telegram) |
| [`internal/managers/telegram`](#pkg-telegram) | Telegram bot manager: session↔topic mapping, commands, rendering |
| [`internal/managers/cli`](#pkg-managercli) | Built-in local chat: the reserved `coagent` project, chat ops on the control socket, `channel=cli` sessions |
| [`internal/ctl`](#pkg-ctl) | Control socket: JSON-RPC 2.0 transport, op registry, server pushes, single-instance flock, `status` |
| [`internal/configops`](#pkg-configops) | Config mutation ops: raw-draft discipline, guards, verdicts, atomic write + backups, the pending-apply marker |
| [`internal/install`](#pkg-install) | Service registration: systemd unit / launchd plist, binary placement, lifecycle verbs |
| [`internal/version`](#pkg-version) | The build-stamped binary version — a leaf everything reports from |
| [`internal/config`](#pkg-config) | Environment + unified YAML configuration |
| [`internal/coagenthome`](#pkg-coagenthome) | Coagent home: `~/.coagent` resolution + the name of everything inside it — the single owner |
| [`internal/logger`](#pkg-logger) | Structured logging via zap |
| [`internal/controllerapi`](#pkg-controllerapi) | Private Controller interface + request/response DTOs, `SessionNotification`, `State*` constants — the leaf contract built-in managers use |
| [`internal/sessionstore`](#pkg-sessionstore) | Session + message persistence (SQLite): `Store`, `SessionRecord`, `StoredMessage`, delivery CAS — the schema's sole owner |
| [`internal/llmwire`](#pkg-llmwire) | LLM wire vocabulary: Message, Response, ToolCall, ToolSchema, MessageUsage, ChatOption, role constants |
| [`internal/sessionevent`](#pkg-sessionevent) | Session→controller event vocabulary: Notification, NotificationType, Notify* constants |
| [`internal/id`](#pkg-id) | Random ID generation for envelope/tool-call/todo-item identifiers |
| [`internal/git`](#pkg-git) | Git CLI wrapper: marketplace clone/pull + worktree isolation |
| [`internal/migrate`](#pkg-migrate) | SQLite open + goose migration runner |

## Rules for new code

**These are prescriptive.** Existing code may violate some of these — that's tech debt, not a precedent. New code MUST follow them.

### State synchronization
1. **Persist to DB before any side effect.** Side effects: PubSub publish, in-memory state update. If persist fails, abort — don't fire-and-forget.
2. **If in-memory update fails after DB success, return the error.** The caller must know the change is deferred. Don't swallow it with a warning log.
3. **On crash, only DB state survives.** Design every feature so that resuming from SQLite checkpoint produces correct behavior. If a piece of state can't be re-derived from DB, it must be persisted.

### Concurrency
4. **Never hold a mutex during IO, DB calls, network calls, or LLM calls.** Copy the data you need under the lock, release the lock, then do IO. Every mutex in the project follows this — see per-package ARCHITECTURE.md mutex inventories.
5. **Every goroutine must have a defined shutdown path.** Either via context cancellation, a done channel, or scoping to a function call. Detached `go func()` with `context.Background()` is a leak.
6. **Check-then-act across lock boundaries is a TOCTOU race.** If you unlock between checking and acting, another goroutine can change the state. Use double-check patterns (check → unlock → do work → re-lock → re-check → commit) or hold the lock for the entire check-and-act.

### Error handling
7. **New code MUST propagate errors.** Existing `_, _ = db.Exec(...)` patterns are tech debt. New Store methods, handlers, and business logic must check and return errors.
8. **Use sentinel errors, not string matching.** Define `var ErrNotFound = errors.New("not found")` and wrap with `%w`. Callers use `errors.Is()`. Never compare `err.Error()` strings.
9. **Never discard `json.Marshal`/`json.Unmarshal` errors.** If serialization fails, the data is corrupt — return the error or log and skip the operation.

### Resources
10. **Every resource acquired must have a corresponding cleanup on every exit path.** `session.Close()`, semaphore release, channel close, goroutine cancellation. Use `defer` for cleanup. Missing cleanup compounds over time (leaked MCP connections, semaphore slot exhaustion).

## System overview

Coagent is a self-hosted headless coding agent. Unlike interactive coding assistants (Claude Code, OpenCode) that require a human at the keyboard, coagent runs as a daemon and executes tasks unattended. Telegram control is built in via the managers runtime.

Internally, each task runs as an isolated session with its own LLM client, tool registry, and conversation history. Sessions persist to SQLite and survive crashes.

Built-in managers talk to the daemon **in-process** through contracts in `controllerapi`: Telegram uses the complete `Controller` aggregate, while the built-in CLI accepts only `ChatController`. This is a private composition boundary, not a public SDK, plugin ABI, or control-socket protocol. There is no inbound **network** surface: the one thing the process listens on is `~/.coagent/daemon.sock`, a same-user unix socket at mode 0600 (`internal/ctl`). That widens nothing — a process that can open it already runs as the daemon's user and can do everything the daemon can — but the rule is "no network listener", not "no listener", and the distinction is worth stating.

```
Telegram manager (embedded)
  │ controllerapi.Controller (in-process)
  ▼
managers (Runtime)
  │
  ▼
daemon (Service)
  │  session lifecycle, concurrency, subagent LinkStore
  ▼
session ── per-task orchestration + ReAct loop: wires LLM + tools + MCP + memory,
           runs LLM call → tool execution → observation → repeat
  │
  ├── llm ── LLM client (Anthropic, Google, OpenAI-compatible)
  ├── tool/builtin ── built-in tools (bash, read, write, edit, grep, etc.) + BuildStack
  ├── mcp ── external tool servers (MCP protocol)
  ├── registry ── agent-type taxonomy, per-session Set
  ├── llmwire ── LLM wire types (Message, Response, ToolSchema, MessageUsage, roles)
  ├── sessionevent ── session→controller event type (Notification)
  ├── sessionstore ── session + message persistence (SQLite)
  └── memory ── curated per-project memory (CuratedStore)
```

**Composition root**: there is no DI framework. `cmd/coagent/main.go` constructs every component by hand, in dependency order, and records a named stop closure for each one as it starts; shutdown replays those closures in reverse. See [Initialization](#initialization-sequence) and [Shutdown](#shutdown-sequence).

## Package dependency graph

Dependencies flow strictly downward. No circular imports exist.

```
                    ┌───────────────────────────────────────────────┐
                    │                  cmd/coagent                   │
                    │   composition root -- manual wiring, no DI     │
                    └─┬──────────┬───────────┬───────────────────────┘
                      │          │           │
                      ▼          ▼           ▼
                  managers                daemon ──► controllerapi, loader,
                      │                      │        registry, schedule, session,
                      ▼                      │        sessionstore
                  telegram                   │        mcp, mcpstore
                      │                      ▼
                      ▼                   session ──► builtin, llm, loader, mcp,
                controllerapi                │         mcpstore, memory, registry,
                                             │         sessionstore, todo
                      │                      │
                      ▼                      ▼
                sessionevent              builtin ──► bashsandbox, loader, lsp, mcp, memory, shellenv, todo
                                                        (bashsandbox, lsp, mcp ──► shellenv)
                                          llm ──► catalog

  Shared leaves built in main() and passed down: mcp.Pool, loader.MarketplaceCache,
  git.Client, sessionStore (sessionstore), scheduleSvc, session.Factory, shellenv.Provider.

  Protocol/utility imports are explicit edges too: there is no global
  "common component" bypass in the architecture allowlist.
```

The tree under `internal/` is deliberately **flat** — package names say what they
provide, and dependency tiers are modeled ONLY by `.go-arch-lint.yml` plus the
`mayDependOn` table below, never by directory nesting. The two remaining nestings
(`tool/builtin`, `managers/telegram`) encode an implements / variant-of
relationship, not a tier.

**Rule**: Dependencies flow downward per the table below. There are no global
`commonComponents`: importing even `logger`, `config`, `tool` or a protocol DTO is
an explicit reviewed edge. Packages at the same tier never import each other
unless the table grants it. Cross-tier references that would otherwise cycle use
consumer-defined interfaces (see [Interface boundaries](#interface-boundaries)).
This mirrors `.go-arch-lint.yml` exactly; `ignoreNotFoundComponents` is false and
`deepScan` is true, so a stale component or a dependency hidden behind injected
concrete values fails `make arch`.

| Package | Complete `mayDependOn` allowlist |
|---------|----------------------------------|
| `config` | `coagenthome` |
| `controllerapi` | `sessionevent` |
| `install` | `coagenthome` |
| `migrate` | `coagenthome`, `logger`, `migrations` |
| `todo` | `id` |
| `bashsandbox` | `coagenthome`, `shellenv` |
| `lsp` | `coagenthome`, `logger`, `shellenv` |
| `loader` | `coagenthome`, `config`, `git`, `logger` |
| `mcp` | `logger`, `shellenv`, `tool` |
| `shellenv` | `coagenthome`, `logger` |
| `tool` | `llmwire` |
| `catalog` | `coagenthome`, `config`, `logger` |
| `llm` | `catalog`, `config`, `llmwire`, `logger` |
| `builtin` (`tool/builtin`) | `bashsandbox`, `coagenthome`, `config`, `loader`, `logger`, `lsp`, `mcp`, `memory`, `shellenv`, `todo`, `tool` |
| `session` | `builtin`, `config`, `git`, `id`, `llm`, `llmwire`, `loader`, `logger`, `mcp`, `mcpstore`, `memory`, `registry`, `sessionevent`, `sessionstore`, `shellenv`, `todo`, `tool` |
| `schedule` | `logger`, `sessionevent`, `tool` — never `session` or `daemon`; callbacks use its own `SessionSender` |
| `telegram` (`managers/telegram`) | `coagenthome`, `config`, `controllerapi`, `logger`, `sessionevent` |
| `managercli` (`managers/cli`) | `controllerapi`, `ctl`, `logger`, `sessionevent` |
| `managers` | `config`, `controllerapi`, `logger`, `telegram` |
| `daemon` | `coagenthome`, `config`, `configops`, `controllerapi`, `git`, `id`, `llmwire`, `loader`, `logger`, `mcp`, `mcpstore`, `registry`, `schedule`, `session`, `sessionevent`, `sessionstore`, `tool` |
| `configops` | `coagenthome`, `config`, `logger` |
| `ctl` | `coagenthome`, `config`, `logger`, `version` |
| `main` (`cmd/coagent`) | any (composition root) |

`coagenthome`, `logger`, `llmwire`, `sessionevent`, `id`, `git`, `version`,
`registry`, `memory`, `mcpstore`, `sessionstore`, and `migrations` import no
project component. The key acceptance property remains: production packages
outside `cmd/coagent` and `daemon` do not import `daemon`; built-in managers use
the private `controllerapi` contract instead.

## Package ownership

Each package owns one concern. When in doubt about where code belongs, this table is the answer.

| Package | Owns | Does NOT own |
|---------|------|-------------|
| `daemon` | Session lifecycle (create, resume, kill), project-identity persistence (SQLite project registry) + subagent-link ledger (`LinkStore`), PubSub events, admission control, the in-process implementation of the `controllerapi.Controller` contract, the config/`request_secret` tools and the in-memory staged-call ledger that answers them | The `Controller` API contract + its DTOs (owned by `controllerapi`), session/message persistence (owned by `sessionstore`), session internals, ReAct loop, tool execution, schedule firing (owned by `schedule`) |
| `session` | Per-task wiring (LLM + tools + MCP + memory), resolving the project's MCP definitions and expanding their `${VAR}` env against the secrets map, ReAct loop, leading `/skill <name>` expansion, conversation history (in-memory via `messageStore`, persisted through `sessionstore.RuntimeStore`), compaction as the single automatic context-pressure response (tool-result clearing is its first phase, skill reattachment its last), loop detection, tool execution orchestration, prompt building, model switching, gating authority (`RegisterGatedTool`), the `compact_context` tool | Tool implementation (delegates to `builtin`), LLM implementation, session/message SQLite persistence (owned by `sessionstore`), subagent spawning/admission (owned by daemon) |
| `registry` | Agent-type taxonomy (`AgentType`, `AgentTypeConfig`), built-in types (build, general, explore, compaction), immutable per-session `Set` (built-ins + project subagents), prompt templates | Agent execution, tool filtering beyond `Set.FilterTools`, session wiring |
| `llmwire` | The LLM wire vocabulary: Message, Response, ToolCall, ToolSchema, MessageUsage, role constants, and `ChatOption`/`WithMaxTokens` (per-call request narrowing) | Behavior beyond resolving its own options — otherwise types only. What a carried field *means* belongs to the package that acts on it |
| `sessionevent` | The session→controller event vocabulary plus `Notification.Validate`, the discriminator/field invariant at the publish boundary | Routing, persistence, controller behavior |
| `controllerapi` | The `Controller` interface + all request/response DTOs (`SessionCreateData`, `SessionInfo`, …), `SessionNotification`, `State*` constants — the leaf contract managers program against | The implementation (owned by `daemon`), any behavior |
| `sessionstore` | Session + message persistence in SQLite: `Store`, `SessionRecord`, `StoredMessage`, atomic compaction replacement/order, atomic child+initial-link creation, the delivery CAS (`DeliverCompletionAtomic`), scheduled-delivery identity + transcript transactions — the schema's transaction owner | Subagent-link lifecycle CRUD after creation (owned by `daemon.LinkStore`), the ReAct loop, in-memory history |
| `id` | Random ID generation (`Generate()`) for envelope IDs, tool-call IDs, todo-item IDs | Persistence-backed IDs (those are DB auto-increment) |
| `llm` | The private `driverProtocol` interface + registry, catalog endpoints and wire-format parsers, client lifecycle, retry logic, catalog enrichment (`EnrichCatalog`) and its validation, cost tracking, reasoning-effort mapping | Message format (uses `llmwire.Message`), tool format (uses `llmwire.ToolSchema`, not `tool.Tool`), HTTP fetching/caching (owned by `catalog`) |
| `catalog` | Fetching and disk-caching a driver-supplied `Source`; model-id matching; the effort vocabulary; `ModelSpec` | Which catalog a driver uses, its URL, its payload format, and writing anything onto config (all owned by `llm`) |
| `tool` | `Tool`/`Result`/`Registry` contract, `ErrSuspend`, call-ID context helpers, tool-ID constants, `SleepParams`/`ParseDuration`, tool-result truncation budget | Tool implementations (owned by `builtin`), any cross-package DI interface (schedule/daemon/session own their tools directly now) |
| `bashsandbox` | Constructing commands with optional native direct-filesystem-write confinement; writable-root validation and backend preflight; sourcing the per-cwd `shellenv` snapshot for user shell commands (`ShellCommand`) | Tool mutation semantics and output/timeout handling (owned by `builtin`), network or secret isolation, the snapshot itself (owned by `shellenv`) |
| `shellenv` | Per-cwd login+interactive shell snapshot: capture, fingerprint-validated cache (30-min backstop, per-instance salt, `0600`/`0700`), replay via `WrapExec`/`ShellCommand`, `Fingerprint`/`Invalidate`; the security invariant (capture inherits `os.Environ()` only, never a secrets map) | Deciding when to spawn (consumers do), the sandbox policy (owned by `bashsandbox`), any config/secrets access |
| `builtin` (`tool/builtin`) | Built-in tool implementations, `BuildStack`/`StackConfig`/`Stack` (registry+LSP+MCP wiring for session) | Tool registration order at the call site, MCP/LSP internals (delegates) |
| `mcp` | MCP server *connections*: pooling, refcounts, TTL reaping, `Evict`, tool discovery, `AcquireForWorkDir` (pool-or-direct acquisition) | Which servers exist (owned by `mcpstore`), `${VAR}` resolution (done by `session` before acquire), tool execution (delegates to registry after registration) |
| `mcpstore` | The MCP registry: global/project-scoped server definitions in the `mcp_servers` table | Connection lifecycle (owned by `mcp`), secret resolution (env references are stored literally), the management tools (owned by `daemon`) |
| `memory` | Curated-memory CRUD (`CuratedStore`) over the `memories` table | Tool wiring (`builtin` owns the `memory_*` tools) |
| `loader` | CLAUDE.md parsing, SKILL.md loading, subagent definitions, marketplace git cloning | Skill execution (tools handle that), git internals (delegates to `git.Client`) |
| `schedule` | Schedule storage (SQLite), CRUD operations, cron validation, one-shot/recurring scheduling, executor that fires pending schedules, the `schedule` and `sleep` tools | Session execution (delegates via `SessionSender`), removal-on-kill orchestration (daemon calls `RemoveAllForSession`) |
| `todo` | In-memory task list for the agent | Persistence (session persists via Store on checkpoint) |
| `lsp` | LSP server lifecycle, code intelligence queries | File I/O (tools do that) |
| `migrate` | SQLite database opening, goose migration runner | Migration definitions (each service provides its own), business logic |
| `managers` | Manager runtime: reads `UnifiedConfig.Managers`, builds/starts each configured manager, tracks which are still alive and records per manager why it stayed down (`RunningIDs`/`StartError`) | Manager-specific logic (delegates to `telegram`, `cli`), the built-in CLI manager's lifecycle (main starts that one directly) |
| `managers/telegram` | Telegram bot manager: session↔topic mapping, commands, voice, rendering, subscribes to `controllerapi.SessionNotification` | Session lifecycle, daemon persistence |
| `managers/cli` | The built-in local chat: get-or-create of the reserved `coagent` project, the `chat_open`/`chat_send`/`chat_stop` ops, forwarding that session's events to attached terminals, stamping `channel=cli` | The transport (owned by `ctl`), session lifecycle (drives only `controllerapi.ChatController`) |
| `ctl` | The control socket: framing, JSON-RPC dispatch, the op registry other layers register into, server→client pushes, the single-instance flock, and the one built-in op (`status`) | What any registered op *means* — config mutation is `configops`, chat is `managers/cli`, the bootstrap handlers live in `cmd/coagent` |
| `configops` | Every semantic config mutation: the raw (unresolved) draft, the guards, `Verdict`, backup + atomic write + retention, the secrets-file line editor, and the pending-apply marker with its boot-time resolution | Deciding *when* to apply or restart (owned by `daemon.ConfigApplier` and `cmd/coagent`), the config schema itself (owned by `config`) |
| `install` | Service registration: rendering the systemd unit or launchd plist, placing the binary, and the install/uninstall/start/stop/restart verbs | Anything the daemon does once running; it never imports another internal package |
| `version` | The build-stamped version string | Everything else — it is a `var` and a comment |
| `cmd/coagent` | Process entry point, manual construction of every component in dependency order, named-stop shutdown accumulator | Business logic (delegates everything to service packages) |


## Interface boundaries

Packages never import each other's concrete types across boundaries. Primary
package capabilities are exposed through producer-side interfaces (`Service`,
`Store`, `Factory`, `Registry`). Narrow consumer-side role interfaces are used
only where they break import cycles or deliberately expose a tiny role. A broad
producer aggregate may also embed producer-owned capability interfaces: consumers
accept the smallest stable capability they actually use, while the constructor can
still return one complete implementation contract.

### How circular dependencies are avoided

Two key patterns:

**1. Narrow consumer-defined interfaces at the composition root** — When the composition root needs to hand a lower-level package a way to call back into a higher-level one without an import cycle, the *lower* package defines a narrow interface and `main.go` pins the higher-level concrete type to it with a `var x Iface = concrete` line:
- `schedule.SessionSender` — defined by `schedule`, implemented by `daemon.Service`; a compile-time assertion lives beside `daemon.svc`, and assignment into `core.scheduleSender` pins the composition-root wiring in `startCore`.
- `applyVerdictSender` and `secretRequestResolver` — narrow roles in `cmd/coagent`; `core` stores them separately from `schedule.SessionSender`, so config RPC handlers and restart verdict delivery never receive the complete daemon service. `secretRequestResolver` is the whole masked-prompt lifecycle (answer, decline, list what a session is still waiting on) and embeds `cli.SecretRequests`, the consumer-defined half the chat manager needs.

**2. Control-plane tools live in the package that implements their behavior, not in `tool`** — Since the July refactor, `tool` no longer hosts DI interfaces like the former `tool.Spawner`/`tool.ScheduleStore`/`tool.Compactor`/`tool.CuratedMemoryStore`. Each tool that needs domain state now lives directly in the package that owns that state, and depends on it directly (same-package field access) or via a tiny **private** interface scoped to that package:
- The `task`, `get_subagent_result`, `send_to_subagent` tools live in `daemon` (`task.go`, `subagent_result.go`, `subagent_send.go`) and call an unexported `spawner` interface implemented by `daemon.svc` itself.
- The `schedule` and `sleep` tools live in `schedule` (`schedule_tool.go`, `sleep.go`) and hold `schedule.Service` directly.
- The `compact_context` tool lives in `session` (`compact_context.go`) and calls an unexported `compactor` interface implemented by `session.svc` itself — the doc comment on it is explicit: "the session (\*svc) implements it directly, so no cross-package interface is laundered back into the tool package."
- `memory_save`/`memory_delete` (in `tool/builtin`) hold `memory.CuratedStore` directly — no `tool.CuratedMemoryStore` indirection.

`tool` itself is left as a **pure protocol leaf**: `Tool`/`Result`/`Registry` interfaces, `ErrSuspend`, the `WithCallID`/`CallIDFromContext` context helpers, tool-ID constants, `SleepParams`/`ParseDuration`, and the truncation-budget constants. Every package that defines a tool has an explicit `.go-arch-lint.yml` edge to it; a new consumer requires an allowlist change.

**3. Shared types split by role across two leaf packages** — What used to be one `dto` package is now two independent, single-vocabulary leaves. `llmwire` holds the **LLM wire protocol** (`Message`, `Response`, `ToolCall`, `ToolSchema`, `MessageUsage`, role constants), consumed by `llm`, `tool`, and `session`. `sessionevent` holds the **session→controller event protocol** (`Notification`/`NotificationType`/`Notify*`), consumed by `session`/`schedule` → `controllerapi`/`daemon` → `telegram`; `llm` has zero consumers of it. Keeping them apart means `llm` never sees a `Notification` and `telegram` never sees a `Message`. `llm.Client.Chat` takes `[]llmwire.ToolSchema` (via `tool.ToSchemas`), not `[]tool.Tool` — this is what lets `llm` depend on nothing but `config`, `llmwire`, and `logger`, with zero dependency on `tool`.

**4. Built-in managers program against a private leaf contract (`controllerapi`)** — `Controller`, its `ChatController` capability and every request/response DTO live in `controllerapi`, a leaf that imports only `sessionevent`. `daemon` implements the complete aggregate (`var _ controllerapi.Controller = (*controller)(nil)`); Telegram genuinely uses nearly all of it, while CLI accepts only `ChatController`. Adding discovery/admin operations therefore cannot break the CLI harness. Managers import `controllerapi`, never `daemon`. The package is under `internal/` deliberately: it is not a supported third-party extension boundary.

**5. Persistence capabilities encode authority (`sessionstore`)** — `NewStore` returns the aggregate; live transcript code receives `RuntimeStore`, the durable input seam receives `InboxStore`, and daemon lifecycle uses `OrchestrationStore`. One implementation owns SQLite transactions that cross those capabilities.

### Interface map

| Consumer | Interface | Implementor | Why it exists |
|----------|-----------|-------------|---------------|
| managers/telegram | `controllerapi.Controller` | `daemon.controller` (pinned in `main.go`) | Telegram exercises the rich control surface without importing daemon |
| managers/cli | `controllerapi.ChatController` | `daemon.controller` | local chat sees only create/send/list/stop/project/event-stream capabilities |
| schedule | `schedule.SessionSender` | `daemon.Service` (compile-time assertion in daemon, wiring pinned in `main.go`) | schedule's executor notifies sessions without importing daemon |
| daemon | `session.Factory` | `session.factory` | daemon creates sessions without knowing internals |
| session | `sessionstore.RuntimeStore` | `sessionstore.store` | a live session may mutate its transcript/checkpoints, not orchestrate other sessions |
| daemon | `sessionstore.OrchestrationStore` + `InboxStore` | `sessionstore.store` | daemon owns lifecycle and durable input admission, not loop checkpoints |
| llm | `catalog.Fetcher` | `catalog.fetcher` | all four drivers share one fetcher, memoized per URL; tests inject a fake returning canned bodies instead of reaching the network |
| session, daemon | `mcpstore.Store` | `mcpstore.store` | session reads the registry at stack build, daemon's tools write it — neither owns it |
| daemon | `mcp.Pool` | `mcp.pool` | the MCP tools evict a removed server's subprocess without owning pool lifecycle (`main` does) |
| daemon | `daemon.LinkStore` | `daemon.linkStore` | producer-side; daemon persists its own subagent ledger |
| session | `compactor` (private) | `session.svc` | `compact_context` tool triggers compaction without a cross-package interface |
| daemon | `spawner` (private) | `daemon.svc` | subagent tools spawn, inspect, and re-engage children without a cross-package interface |

## Agent types and delegation

### Two-type taxonomy

Coagent uses a two-type subagent taxonomy split on a single axis: **can the subagent modify the codebase?**

| Type | Capability | Tool access | Safety |
|------|-----------|-------------|--------|
| **general** | Full — read, write, edit, bash, webfetch, MCP, all tools | `*` minus todoread/todowrite | Can cause damage — requires clear prompt |
| **explore** | Read-only — read, grep, glob, ls, bash (read-only commands) | Restricted set | Structurally safe — cannot modify files |

This binary split is intentional. In a headless (unattended) system, there is no human to interrupt a subagent that starts corrupting files. Explore subagents are structurally safe regardless of LLM behavior. Adding more types (e.g., "web-research", "test-runner") would create combinatorial complexity without improving safety or capability — general already covers all non-read-only work.

### Delegation model

The build agent (primary) delegates to subagents when:
1. The work would consume significant context window (3+ files to read, multi-file edits)
2. Work items are independent and can run in parallel
3. A clean context is preferable to accumulated history (fresh investigation)

The build agent does NOT delegate when:
1. The task is 1-2 tool calls total
2. Steps are strictly sequential with data dependencies between them

### Subagent isolation

Subagents have zero access to the parent's conversation history. The build agent must include all necessary context in the prompt. Subagents share the parent's working directory, MCP connections, and memory service, but have their own LLM client, tool registry, and message history. Each subagent is a first-class session row created together with its durable `subagent_links` row by `sessionstore.OrchestrationStore.CreateSubagentWithLink`; the two rows commit or roll back as one aggregate. Subagents inherit the parent's AGENTS.md content — it is injected as the first user message on fresh runs, providing project context.

`send_to_subagent` is the single continuation API for both foreground and background children. A follow-up preserves the same child ID and transcript. If the original foreground task call is already resolved, re-arm changes the link to non-blocking and the next completion reaches the parent through `subagent_event`; the `task` tool has no separate resume-by-ID parameter.

### Custom subagents

Users define custom subagents via markdown files with YAML frontmatter in `.claude/agents/` or `.coagent/agents/`. Custom subagents specify their own tool list, prompt, description, and optional model override; a definition is read as generously as it can be ([ADR-0014](docs/adr/0014-subagent-definitions-degrade-never-disable.md)) — an omitted `tools:` inherits the full inventory, and a `model:` the catalog cannot resolve is dropped with one warning at load rather than failing every spawn. During session setup (before agent-type resolution), `loader.LoadSubagents` loads these files and `subagentConfigs()` converts `loader.Service.ListSubagents()` into `[]registry.AgentTypeConfig`; `registry.NewSet(projectSubagents)` then overlays them onto the built-in types to produce the session's immutable `*registry.Set` — a project subagent may shadow a built-in of the same name. They appear alongside built-in types in the task tool's enum via `Set.ListSubagents()`.

### Concurrency

Concurrency is governed by the daemon's admission controller (`internal/daemon/admission.go`): up to 16 total concurrent sessions, 12 children, 8 in-flight per parent, and a nesting depth of 3. Background-subagent overflow queues FIFO and drains as slots free. A blocking parent suspends while its child runs — its loop exits, holding zero run-slots — and is re-engaged once the child completes.

### Description design principles

Agent type descriptions are shown to the LLM in the task tool schema to decide which type to spawn. They must:
- Lead with **capabilities** (what the type can do), not role labels
- Include the **constraint** (explore: "cannot modify files") as a decision criterion
- Avoid numeric heuristics — those belong in the build agent prompt, not in type metadata
- Work across LLM backends (Claude, Gemini, GPT, DeepSeek) — use concrete capability lists, not abstract roles

## Cross-cutting architectural decisions

These apply everywhere. Violating them causes bugs that are hard to trace.

### SQLite is the sole source of truth

All persistent state lives in SQLite. In-memory structures (`Manager.loops`, `runner.service`, `Inbox`, `todo.items`) are ephemeral caches or coordination primitives. When DB and memory disagree, DB wins. Sessions are designed to resume from SQLite checkpoint — any in-memory state that wasn't persisted is intentionally lost on crash.

**Implication**: Every write path must persist to DB first, then update in-memory. Never the reverse.

### Interface-first at package boundaries

Every package exports an interface, not a struct. Constructor `New()` returns the interface. Implementation struct is private. Compile-time check: `var _ Interface = (*impl)(nil)`.

**Implication**: Tests can mock any dependency. Packages are replaceable.

### No package under `internal/` imports another's concrete types

Dependencies flow through interfaces defined by the consumer. This keeps the dependency graph acyclic and each package independently testable.

**Exceptions**:
- `session` imports `loader.MarketplaceCache` (a `loader`-defined interface) directly (passed through `SharedDeps`).
- `session` imports `*builtin.Stack` (from `tool/builtin`) as a concrete type, returned directly by `builtin.BuildStack`. This is deliberate: `builtin` is documented in `.go-arch-lint.yml` as the "capability hub" — the one place allowed to reach into `loader`/`lsp`/`mcp`/`memory`/`todo` to assemble a tool registry — and `Stack.Close()` (LSP + MCP teardown) is a concrete lifecycle method, not a swappable behavior worth hiding behind an interface.
- `sessionstore.SessionRecord` / `sessionstore.StoredMessage` are shared concrete types by design; the same leaf also owns `RuntimeStore`/`OrchestrationStore` capabilities. A row struct crossing a boundary is not an inversion candidate — there is no behavior to hide.

### Config consumption: `*config.Config` vs `*config.UnifiedConfig`

The full `*config.Config` (CLI/env fields, subagent config, the `*UnifiedConfig` pointer) is a per-process/per-session wiring object, not a generic service dependency. A package that only needs provider/model lookups should take `*config.UnifiedConfig` (or plain scalars), not the whole `*config.Config`.

**Precedent**: the `llm` client structs (`anthropicClient.cfg`/`openaiClient.cfg`) were narrowed from `*config.Config` to `*config.UnifiedConfig` — neither ever needed per-session `WorkDir`/`Model` overrides, only pricing/provider config. `mcp.AcquireForWorkDir` was already `*config.UnifiedConfig`-only.

**Who legitimately still holds the full `*config.Config`**: the composition root (`cmd/coagent`); `session.factory`/`session.svc` (each session clones it per `WorkDir`/`Model`); and the daemon/managers wiring layer (`daemon.controller`, `daemon.New`, `managers.Runtime`) that reads top-level scalars (`DefaultModel`, `SubagentModel`, `UnifiedConfig.Managers`) at construction time. Everything below that — `llm`, `memory`, `mcp` — takes `UnifiedConfig` or scalars, never the full struct.

## Data lifecycle

### Conversation message: birth → compaction

```
1. ACCEPT
   Controller.{CreateSession,SendSessionMessage}
     → svc.Send / svc.SendToSession
       → sessionstore.EnqueueInput(prompt)  ← durable before acknowledgement

2. ENTER SESSION
   runner.runSession → factory.Create(InputBoundary) → RunDaemon
     → settle the preceding assistant/tool state
     → InputBoundary.Peek → session.PrepareUserMessage
       → leading `/skill <name> [args]` becomes a canonical `<skill>` envelope
     → sessionstore.PromoteInput
       → insert `messages` row + mark inbox accepted + mark session active in one transaction
       → reload the in-memory transcript

3. LLM CALL
   session.runLoop → callLLM(messages + tools)
     → LLM response → messageStore.addAssistantMessage(response)
       → store.PersistMessage(msg)

4. TOOL EXECUTION
   session.executeToolCalls → tool.Execute
     → messageStore.addToolResult(toolCallID, result)
       → store.PersistMessage(msg)
   → back to step 3 (loop)

5. COMPACTION (when the projected request size crosses the threshold)
   session.applyContextEvents → refuses while any tool call is pending
     → explicit flag set, or projection > compactionFraction × window?
       → session.compact(keepRecent)
       → applyClear (phase 1: drop tool-result bodies older than keepRecent rounds)
       → header + system prompt still under the threshold? else errCompactionHeaderTooLarge
       → ONE LLM summarization call, capped at compactionOutputReserve
       → select latest invocation per skill, within 5K/10%-of-window token budgets
       → rebuild canonical transcript: [header] + [summary] + [ack] + [primer] + [skills]
         (the summary turn carries brief + verbatim excerpt + active-subagent section;
          no verbatim tail survives)
       → store.ReplaceCompactedMessages(...)    ← soft-delete + stable-ID reposition + inserts in one tx

6. SESSION END
   loop exits → session.persistState("completed"/"suspended"/"error")
     → updates iteration, todo_items, status in SQLite
   → runner: session.Close() → delete from loops → release semaphore

7. RESUME
   new runner for same session → factory.Create(opts with ResumeMessages)
     → store.LoadActiveMessages() ← loads non-compacted messages in persisted transcript order
     → messageStore.setMessages(loaded) ← restores in-memory history
     → session checks last message: pending tool calls? execute them. text? deliver and exit.
```

### State checkpoints

After each agent iteration, `session.persistState()` writes to SQLite:
- `iteration` counter
- `todo_items` JSON
- `status` ("active" during run, final: "completed"/"suspended"/"error")
- `compaction_brief` (after compaction)

This is the resumability mechanism — on crash, the session resumes from the last checkpoint.

## Error propagation strategy

### Retried (transient)
LLM calls go through `llm/retry.go` with bounded retry (6 attempts) + exponential backoff:
- Rate limits (429), server errors (500-504), network errors (timeout, connection refused)
- **Unknown errors are retried by default** (conservative)

### Fatal (stop the agent loop)
- LLM error after retries exhausted → notifies user "LLM error: ..." → exits loop
- Context cancellation → returns `ctx.Err()` immediately
- Max iterations reached → exits loop
- Iteration callback failure (persist error) → exits loop

### Surfaced to the user (via PubSub notification)
- LLM failures, compaction status, session errors, final responses

### Silently swallowed (logged but not propagated) — TECH DEBT
> These are existing patterns, NOT a template for new code. New code must propagate errors (see "Rules for new code" §7).

- Message persistence failure → warning log, message stays in-memory only
- Compaction brief persistence → warning log
- Notification delivery failure → warning log
- Tool execution errors → formatted as text in tool result, LLM sees it and reacts
- Tool panics → recovered, formatted as error result
- MCP acquisition failure → warning log, session created without MCP tools

### Special: ErrSuspend and pending external calls
Tool returns `tool.ErrSuspend` → tool result NOT recorded → agent exits cleanly with `Suspended=true` → session persisted as "suspended" → on resume, daemon injects the real tool result.

The set of tools that can do this is `tool.IsExternalCall`: sleep, task, the config
mutations and `request_secret`. Their pending calls are protected on three fronts —
`handlePreviousResult` never re-executes one, `repairTranscriptExcluding` never
stubs one, and a user message that arrives mid-flight is moved to the
manager-owned deferred backlog rather than appended, because a user turn between
the `tool_use` and its `tool_result` is a transcript no provider accepts. Only
`ResolvePendingCall(PendingToolCall{ID, Name}, content)` may answer one; it rejects
unknown IDs and mismatched tool names and is idempotent on redelivery.

The protection has two widths on purpose. Repair uses the wide set (by tool name):
over-protecting costs nothing, while stubbing a live call loses a verdict. Blocking
the *loop* uses the narrow set — only calls an authoritative producer ledger says
have already started (`CreateOptions.StagedExternalCalls`) — because a call nobody
staged has never run, and re-executing it is then the correct recovery. On every
session construction the daemon merges three ledgers: config/secret work, sleep
schedule metadata (`tool_call_id`), and blocking child links (`task_call_id`).
Pending-state scans cover the whole active transcript, so a newer synthetic pair
cannot hide an older suspended call.

The two widths must nevertheless *agree* by the time a session runs, and only the
daemon can make them: a call the wide set protects but no ledger owns is one repair
will never stub and nobody will ever answer, so every later request ships a dangling
`tool_use` and the provider rejects it forever. The staged ledger is in-memory, so a
restart produces exactly that state for `request_secret` (and for a config apply
whose marker did not survive). PASS 0 of the startup sweep (`resolveOrphanedCalls`)
is the owner of last resort: it compares the two sets per session and answers the
difference with a deliberate cancellation, without running the loop — see
[ADR-0016](docs/adr/0016-boot-time-cancellation-of-unowned-external-calls.md).

Within one process image the same agreement rule constrains *answering*: the
ledger entry is the ownership a session rebuilt for the delivery checks, so it
may only be dropped after the result is actually inserted (the runner does this,
`injectSessionInput`). A producer that clears its own entry first strands the
call — the suspended session is long closed by then, and the session rebuilt to
take the answer refuses it as unowned.

## Initialization sequence

There is no DI framework. `cmd/coagent/main.go` wires everything by hand, in the exact order below. `run()` installs the signal context and hands argv to `dispatch` (`cli.go`); only the `coagent daemon` verb reaches `bootDaemon`, so the CLI verbs never load config or touch a catalog.

```
main() → run() → dispatch(ctx, os.Args[1:]) → bootDaemon(ctx):
  1. logger.Init(...)          ← zap logger with human encoder + session prefix
  2. cfg, secrets, err := config.NewConfig()
                               ← ~/.coagent/secrets (in-memory, never exported) + env.ParseWithOptions + unified YAML config.
                                 The secrets map is returned, not stored on Config; main threads it to the session factory
  3. logger.SetRedactedValues(cfg.SecretValues) ← registers credential strings scrubbed from all subsequent log output
  4. logConfigStatus(cfg)      ← logs config-load outcome (missing YAML vs. fatal error)
  5. llm.EnrichCatalog(ctx, cfg) ← fetches every driver's model catalog (30s budget for the pass, 5s per fetch) and fills
                                 ModelEntry in place. Fatal: a model whose metadata cannot be resolved names itself and the
                                 missing field. MUST precede any client construction — clients read the enriched fields
  6. probeBashSandbox(cfg)     ← fail-fast: os.Exit(1) if sandbox is configured but the platform backend can't enforce it
  7. runDaemon(ctx, cfg, secrets) ← startup failures print via fmt.Fprintf(os.Stderr, logger.Redact(...)) — stderr bypasses zap

runDaemon(ctx, cfg) — registers a named stop closure (app.onStop) right after each
component starts, so shutdown can replay them in exact reverse order:

  gitClient := git.New()
  provider := shellenv.New()                                   onStop("shellenv", provider.Close)
  pool := mcp.NewPool(provider)                                onStop("mcp.pool", pool.Stop)
  cache := loader.NewMarketplaceCache(gitClient)
  db, err := migrate.Open()                                    onStop("db", db.Close)
  daemonStore := daemon.NewStore(db)
  sessionStore := sessionstore.NewStore(db)
  scheduleStore := schedule.NewStore(db)
  curatedStore := memory.NewCuratedStore(db)
  linkStore := daemon.NewLinkStore(db)
  mcpRegistry := mcpstore.NewStore(db)
  scheduleSvc := schedule.NewService(scheduleStore)
  factory := session.NewFactory(cfg, secrets, curatedStore, sessionStore, gitClient, pool, mcpRegistry, cache, provider)
  daemonSvc := daemon.New(factory, daemonStore, sessionStore, linkStore, scheduleSvc, cfg, mcpRegistry, pool)
  daemonSvc.Start(ctx)                                          onStop("daemon", func(ctx) { daemonSvc.Shutdown(30s) })
                                                                 ← Start() spawns the detached restart-recovery sweep
  controller := daemon.NewController(daemonSvc, cfg, cache, scheduleSvc)
  core := &core{scheduleSender: daemonSvc, ...}                 ← pins daemon to schedule's own interface
  executor := schedule.NewExecutor(scheduleStore, core.scheduleSender)
  executor.Start(ctx)                                           onStop("schedule.executor", func(ctx) { executor.Stop() })
  runtime := managers.NewRuntime(cfg, controller)
  ctlSrv := ctl.NewServer(~/.coagent/daemon.sock, version, Deps{cfg, runtime})
                                                                onStop("ctl.server", ctlSrv.Close)
                                                                 ← registerConfigOps adds set_provider/set_secret/restart_daemon
  go ctlSrv.ServeStarting(ctx)                                   ← accepts from the bind; every op answers CodeStarting for now
  chat := cli.New(controller, ctlSrv, onboardingModel(cfg))     onStop("managers.cli", chat.Stop)
  chat.Start(ctx)                                                ← outside the config-driven loop on purpose:
                                                                   it is how a configless daemon gets a config
  runtime.Start(ctx)                                            onStop("managers", runtime.Stop)
  ctlSrv.MarkReady()                                             ← ops open once every owner has registered;
                                                                   Register rejects all later registrations
  deliverApplyVerdict(ctx, daemonSvc, ops, outcome)              ← answers the tool call that survived a restart,
                                                                   then clears the marker (only after delivery lands)
  select { <-ctx.Done() | <-restart }                           ← SIGINT/SIGTERM, or a config apply asking to come back
  (deferred) a.shutdown(45s-timeout ctx) — see Shutdown sequence
```

Before any of that, `bootDaemon` reads the **pending-apply marker**. It wraps the
*entire* startup validation — config parse, validation, and catalog enrichment —
because those are the failures a pre-write check cannot see. Three outcomes
(`configops.ResolvePending`): the file hash disagreeing with the marker means the
write never landed; a clean boot means applied; a failed boot restores the backup,
re-runs the validation on it, and the verdict names the stage that failed. With no
marker, a config that will not load is fatal exactly as before.
`ResolvePending` no longer clears the marker: it survives until
`deliverApplyVerdict` has durably injected the verdict into the owed session
(bootstrap markers with `SessionID == 0` clear immediately — that verdict reaches
its user only through the reconnecting bootstrap's status read). A failed delivery
keeps the marker; the next boot replays it, and replay against a transcript that
already carries the result is a no-op.

A marker is consumed by **exactly one** boot resolution, which is why the failure
is classified rather than retried blindly. `verdictUndeliverable` reads the owed
session's record: missing, killed, stopping or stopped means no boot can ever
deliver it, so the verdict is logged as undeliverable and the marker cleared;
anything else is transient and keeps the marker for the next boot. A marker kept
forever would arm every later boot, and the first unrelated startup failure
(a delisted model id, an unreachable catalog) would then roll a config that has
been live for days back to its backup. `ClearPending(p)` removes only the marker
instance it resolved — a marker written by a newer apply belongs to that apply's
own waiting call.

A `restart` on the channel returns `errRestartRequested`, which is not a failure:
`bootDaemon` execs the binary at `selfExecPath` (captured via `os.Executable()` at
process start, *before* an update can swap the file) only after the deferred
shutdown has replayed every stop closure — so the new image starts against a
released database, socket and flock. Two things push that channel: a config apply,
and the `restart_daemon` op — which is exactly why a binary update needs no
service manager and no privileges.

### Install and update (`cmd/coagent`)

The CLI verbs never boot a daemon; they either register the service or drive the
socket. Two rules shape them ([ADR-0009](docs/adr/0009-system-daemon-user-binary.md)):

**Sudo appears in exactly one place.** `runDaemonVerb` (`sudo.go`) is the single
escalation gate: every lifecycle verb needs root, so a non-root caller re-execs
`sudo <selfExecPath> daemon <verb>` with passthrough stdio rather than failing on
EACCES. Refusal prints the manual command and exits non-zero — there is no
fallback of any kind. `runServiceAction` below it is privilege-blind and assumes
it already has the rights it needs; the euid read goes through the `geteuid`
package var so the decision is unit-testable.

**Updates never escalate.** `offerUpdate` (`bootstrap.go`) is: confirm →
`install.UpdateBinary()` as the plain user → `install.UnitStale()` warning if the
on-disk unit no longer matches what this version renders → `restart_daemon` over
the socket → poll until the greeting reports the *new* image. The version check
is load-bearing: the old daemon keeps answering on the socket until its drain
closes the listener, so "status answered" alone would accept the image being
replaced. The only path that can reach for a password is the `-32601` fallback,
which means the running daemon predates the op and can only be replaced by a full
install.

**The first provider is not saved until the daemon comes back with it.**
`addFirstProvider` (`bootstrap.go`) reads `status.boot_id` on the same connection
it writes on, then waits for a *different* one (`waitForReboot`, `daemon_wait.go`)
— the pre-restart image answers on the socket until its drain closes the listener,
so "a daemon answered" would hand the chat a socket seconds from being unlinked.
The reconnected daemon's provider list is then the verdict: a config it cannot
boot on was rolled back by `ResolvePending` with nobody to tell (the marker
carries no session), so absence is the only signal, and the bootstrap says so and
exits instead of opening a chat with no provider.

**A committed change restarts whether or not anyone hears the answer.**
`setProvider` and a `setSecret` rotation write first and answer second, so their
restart is scheduled with `Conn.AfterReply` — and `ctl` drops that hook when the
response cannot be written. `restartOnCommit` (`ctl_ops.go`) therefore also arms
the restart on `Conn.Done`: politeness to the caller decides the *order*, never
whether the daemon comes back. Without it a lost reply left the process serving a
superseded config, holding the apply slot, for the rest of its life. The ops take
a `replyHook` rather than `*ctl.Conn` so that pairing is testable without a socket.

**An orphaned credential is the deliberate cost of the write order.**
`commitProvider` writes the provider key into the secrets file and only then
stages and commits the config that references it. The reverse order is not an
option: a `config.yaml` naming a `${VAR}` nothing defines is a fatal load error at
the next boot, while a secret nothing references is inert — `resolveSecrets` only
expands whitelisted sinks it finds *in the config*, and secrets never reach the
process environment. So a `Stage` or `Commit` that fails after the write leaves
the key behind, and that is left alone rather than rolled back: `SetSecret` has no
undo, deleting the line would break an *existing* provider that still references
the same variable (a re-`set_provider` on a known name), and restoring the old
file content would clobber whatever the concurrent secret path wrote in between.
The retry is the cleanup — a provider name maps to one variable
(`SecretVarForProvider`), and `replaceAssignment` rewrites that line in place. The
claim-before-write rule in ADR-0015 still stands for the *refusal* case: a caller
that cannot take the apply slot never writes at all.

**Critical**: `llm.EnrichCatalog` runs before `runDaemon` — before any component exists, let alone any LLM client. Registration order inside `runDaemon` is exactly the reverse of the shutdown order (LIFO), since `app.shutdown` walks the `stops` slice backwards. `daemon.New` must exist before `schedule.NewExecutor` (which is pinned to an interface `daemonSvc` implements). `daemonSvc.Start(ctx)` must run before `managers` start: it finishes sweep PASS 0 **synchronously**, so no controller can open a runner for a session whose unowned external calls are still dangling. The resumes it then spawns are asynchronous and safe to race.

## Shutdown sequence

Triggered by `SIGINT`/`SIGTERM` via `signal.NotifyContext`. `runDaemon` defers `a.shutdown(stopCtx)` (a 45s-timeout context) at entry; `a.shutdown` walks its `[]namedStop` slice **in reverse of registration order** and logs (not fails) on any component's stop error.

```
Exact reverse of the registration order above:

1. runtime.Stop(ctx)            (managers) → each configured manager (e.g. telegram) stops in its own reverse order
1b. chat.Stop(ctx)              (managers.cli) → unsubscribes; ctlSrv.Close drops the terminals
1c. ctlSrv.Close()              (ctl.server) → stops accepting, closes live connections, unlinks the socket
2. executor.Stop()              (schedule.executor) → cancels context, waits for the ticker loop to exit
3. daemonSvc.Shutdown(30s timeout)
   ├── shuttingDown.Store(true)
   ├── copy all runners under mu
   ├── spawn goroutines calling stop() on each runner (WaitGroup)
   │   └── stop() cancels context, waits on runner.done
   │       → runSession returns, defer cleans up (delete from loops, release admission slot, close done)
   └── wait for WaitGroup with timeout
   Result: all sessions stopped.
4. db.Close()                   → close SQLite
5. pool.Stop()                  (mcp.pool) → close all MCP connections
6. provider.Close()             (shellenv) → best-effort remove the per-instance snapshot cache dir
7. lock.Release()               (ctl.lock) → drops the single-instance flock last, so nothing can bind behind us
```

**Why this order matters**:
- Managers stop before the schedule executor and daemon: built-in front ends stop delivering user input before the sessions they'd deliver into disappear
- Daemon shutdown does NOT flush memory: intentional — sessions resume from SQLite on restart
- DB closes after daemon but before the MCP pool: daemon's own `Shutdown` needs the DB to mark runners terminal; the pool has no DB dependency, so it can close last without consequence
- The whole shutdown is bounded at 45s (`runDaemon`'s deferred context); daemon's own drain gets its own nested 30s budget within that

## Cross-package contracts

Rules that span multiple packages and cause runtime bugs when violated.

- **Persist-before-side-effect (daemon, session)**: DB write must complete before PubSub publish or in-memory update. The entire system assumes SQLite is ahead of or in sync with in-memory state, never behind.

- **Session.Close exactly once (daemon → session)**: Every `factory.Create` must be followed by exactly one `Close`. Runner's `runSession` calls Close on every exit path. Missing Close leaks the MCP connection (or pool refcount) and LSP client held by the session's `*builtin.Stack`.

- **Session messages are append-only during execution (session)**: Between `Run()` start and return, no external caller may mutate the message list via `messageStore`. `setMessages()` is only safe before `Run()` (on resume). During execution, only the session's own loop methods (`addUserMessage`, `addAssistantMessage`, `addToolResult`) append messages. Each append is durable-first: the message reaches the in-memory slice only after `InsertMessage` returns its id, so the agent never reasons over a turn the store rejected — the failure is returned and the run aborts.

- **Compaction replacement order is atomic (session → sessionstore)**: `ReplaceCompactedMessages` marks summarized rows compacted, assigns canonical `messages.position` values to retained rows without changing their IDs, and inserts synthetic summary/ack/primer/skill rows in one transaction. Active rows with `position IS NULL` sort after the rebuilt snapshot by ID, preserving messages concurrently committed during compaction; retained IDs keep `subagent_links.delivered_msg_id` valid.

- **Tool results must match tool calls (session → tool)**: Every `ToolCall` in an assistant message must receive exactly one `addToolResult`. Missing results cause the LLM to receive incomplete conversation history. Extra results cause confusion. The session loop enforces this in `executeToolCalls`, which stops at the first result the store refuses and fails the run rather than continuing with a half-recorded batch — the unwritten calls resume as pending.

- **PubSub subscribe-before-send (daemon)**: `CallSession`-style flows must subscribe to PubSub before sending the message. PubSub has no replay — missed notifications are gone.

- **PubSub carries root-session events only (daemon)**: Child (subagent) sessions are invisible to built-in managers end to end — `sessionstore.ListSessions` filters `parent_id = 0`, and every production publish goes through `svc.publish`, which drops notifications whose session has a non-zero `ParentID` (verdict cached in memory; a lookup error fails open). A subagent's outcome reaches its parent through the link ledger, its failures through `notifyChildFailure` publishing on the *parent's* ID. Managers therefore never filter — `sessionevent.Notification` deliberately carries no parent field to filter on. The hub and its `Publish` method are private; `Service.PubSub` exposes only `NotificationSource` subscriptions. `.semgrep/coagent.yml` (`coagent-pubsub-publish-via-gate`) additionally bans direct publish calls outside `publish.go`/`pubsub.go`.

- **ErrSuspend means "don't record this result" (tool → session → daemon)**: When a tool returns `ErrSuspend`, the session must NOT persist the tool result. The daemon injects the real result on resume. Recording a partial result would corrupt the conversation on resume.

- **Every suspended external call has exact identity and a producer ledger (tool → schedule/daemon → session)**: A producer may return `ErrSuspend` only after durably recording which `{tool_call_id, tool_name}` it owes. Sleep stores `tool_call_id` in schedule metadata; blocking task stores it in `subagent_links`; config/secret work uses the staged ledger, and a config apply additionally survives its own restart through the pending-apply marker, which `pendingExternalCallsForSession` merges back in as a fourth producer ledger. Daemon session construction merges those ledgers into `CreateOptions.StagedExternalCalls`. Tool name or "latest assistant turn" is never sufficient evidence of ownership: after a crash an unexecuted call must run, while an already-started call must suspend. A call the transcript shows pending that none of those ledgers claims has lost its producer for good; the startup sweep closes it (PASS 0) rather than leaving a transcript no provider will accept.

- **A session's transcript has exactly one writer — its runner goroutine (daemon)**: completion and schedule results are only *queued* (`rs.inputs`) by their producers and committed in `prepareSessionInputs`, before the synchronous `executeSession`; `settleStoppedCalls` runs only after `rs.stop()` has joined the goroutine. This serialization — not the compaction guard — is what makes a background child completing mid-compaction safe: the commit waits for the iteration boundary, lands as one adjacent pair behind the summary, and `reloadMessages` picks it up.

- **Async transcript input is a sealed sum, not an optional-field bag (schedule/subagent → daemon)**: Exact pending result, blocking completion, background completion, normal tick and fresh task are distinct `sessionInput` variants with validation. Exact resolvers are causally applied before standalone events regardless of arrival order. Standalone events may interrupt sleep by resolving its exact ID first, but cannot jump across a blocking task/config/secret call. Awaited producers receive defer/error and retry without acknowledging work; background completions remain durable in `subagent_links`.

- **Subagent completion, not model-authored sleep, yields a background wait (session/daemon)**: `task` and `sleep` emitted in one assistant response are rejected as a conflicting concurrent protocol before the sleep side effect. On later turns, daemon wraps the sleep tool with a ledger check and rejects it while any child completion is pending delivery. The model may finish its response or continue independent work; the child ledger wakes the parent automatically. Once no child completion is pending, ordinary sleep remains available for external waits.

- **Resolve first, cancel second for sleep interruption (daemon → session/schedule)**: User, subagent and scheduled input interrupt sleep by durably adding the exact original tool result before deleting the pending-sleep row (a one-shot carrying `metadata.tool_call_id`). A crash or cancellation failure after the result is safe: timer redelivery is idempotent. Cancelling first can permanently remove the only wake-up and is forbidden. Standalone one-shot scheduled input has no call ownership and must survive this cancellation.

- **MarkLinkTerminal before UpdateSessionStatus (daemon)**: Every terminalization path (`finalizeChild`, `killSubagent`) writes `daemon.LinkStore.MarkLinkTerminal` **before** `sessionstore.OrchestrationStore.UpdateSessionStatus`. The link is the authoritative signal the restart sweep reads; a crash between the two writes must never leave a status-terminal-but-link-non-terminal child, which would let the sweep resurrect a session that should stay dead. The reverse order is the bug this guards against, not a stylistic preference. Both paths write it through `markLinkTerminalRetrying` (bounded retry) and **both skip the status write entirely when the retry is exhausted**. In `killSubagent` that is the load-bearing part: `MarkSessionKilled` adds `killed_at`, and a child holding `killed_at` with a non-terminal link falls out of *both* recovery queries at once (`ListRunningChildLinks` filters `killed_at IS NULL`, `ListUndeliveredParentLinks` filters `state IN ('completed','error','killed')`) -- no sweep would ever see it again and a blocking parent would wait forever. Skipping the session writes leaves the child in `spawned`/`running` with `killed_at` unset, so the next startup sweep resumes it. The runner is still stopped either way.

- **Child session + initial link are one aggregate (daemon → sessionstore)**: production spawn uses `CreateSubagentWithLink`, which inserts `sessions` and `subagent_links` in one SQLite transaction. The daemon supplies the link vocabulary and sessionstore supplies transaction ownership. `CreateSubagentSession` is row-only setup for persistence tests; using it in a production spawn path would recreate an orphan-on-crash window.

- **`DeliverCompletionAtomic` is the sole writer of `delivered_at`/`delivered_msg_id` (sessionstore)**: The CAS-plus-insert transaction lives in `sessionstore` (`delivery_store.go`), on `sessionstore.OrchestrationStore` — these two subagent-link columns are the one part of the ledger `sessionstore` owns; the rest moved to `daemon.LinkStore`. The method's own doc comment names it the sole dedup for at-least-once delivery. It rejects an empty completion and verifies both `subagent_links.parent_id == sessionID` and the queued completion's internal `activation_seq` before consuming the CAS, then stamps `delivered_at` and inserts the completion message(s) in a single transaction. Re-arm increments `activation_seq`, so a delayed duplicate from an older activation cannot claim the newly cleared delivery marker. A crash commits both marker and messages or neither; a same-activation redelivery loses the CAS and inserts nothing. Any new write path that touches subagent completions must go through this method, never a separate insert-then-stamp sequence.

- **State vocabularies have distinct owners and types**: `sessionstore.SessionStatus` is persisted checkpoint/lifecycle state, `daemon.LinkState`/`LinkOutcome` are the subagent ledger automata, and `sessionevent.State` is ephemeral controller state. Equal serialized words such as `error` are not interchangeable values; cross-layer mapping is explicit. Store methods reject unknown persisted status, link writes reject invalid state/outcome pairs, and notification validation rejects a persisted status masquerading as runtime state.

- **Every production notification validates before routing**: `svc.publish` calls `sessionevent.Notification.Validate` before child lookup or PubSub fan-out. Unknown event types, missing required payload and fields belonging to another event variant are logged and dropped; managers never receive a structurally impossible notification.

- **`schedule.SessionSender` is owned below and pinned above**: `daemon` asserts `var _ schedule.SessionSender = (*svc)(nil)`, then `startCore` assigns `daemonSvc` into the interface-typed `core.scheduleSender` field before `runDaemon` constructs `schedule.NewExecutor`. This is the only production wiring of schedule back to daemon — `schedule` source files never import `daemon`.

- **Scheduled delivery identity commits with transcript mutation (schedule → daemon → session → sessionstore)**: Every standalone one-shot and cron occurrence carries a deterministic delivery ID. `session_deliveries` claims `{session_id, delivery_id, kind, fingerprint}` in the same transaction that inserts the synthetic tool pair or performs a fresh-context reset. Producer acknowledgement may fail after that commit; retry with the same semantics returns `applied=false`, allowing schedule acknowledgement without another model run or controller publication. Reuse with a different fingerprint fails closed. Cron identity and rendered time both use the same minute-truncated occurrence; a fresh-reset fingerprint uses stable `agentsMD + prompt`, never its stamped wall-clock text.

## Keeping this document accurate

This is the **single** architecture document for the repo. There are no per-package `ARCHITECTURE.md` files — each package's internals live in a `##` section under [Package Internals](#package-internals). When a PR changes a package's contract, invariants, state ownership, or concurrency model, update that package's section **in the same PR** (this is [`docs/coding-style.md`](docs/coding-style.md#10-api-evolution--removals) architecture-level §10.4). Stale architecture docs are worse than none.

After implementation changes, verify:

- **Package set matches sections.** `ls internal` should line up with the package sections and the [Contents](#contents) table. A new package needs a new `##` section under Package Internals plus a Contents row and an `<a id="pkg-…">` anchor.
- **Interfaces match "Key types".** Each package exports one interface with a `var _ Interface = (*impl)(nil)` check; the section's Key-types table must list the current interface and its method count.
- **Composition-root wiring matches the lifecycle text.** `New()`/`Start()`/`Stop()` signatures and the `app.onStop` registration order in `cmd/coagent/main.go` should match each section's construction/lifecycle description and the [Initialization](#initialization-sequence) / [Shutdown](#shutdown-sequence) sequences. There is no DI framework — verify against `main.go` directly, not against a wiring module.
- **Dependency direction holds.** No package under `internal/` imports another's concrete types except the documented exceptions (see [Cross-cutting decisions](#cross-cutting-architectural-decisions)); cross-boundary calls otherwise go through consumer-defined interfaces. The documented [dependency graph](#package-dependency-graph) and [interface map](#interface-boundaries) must still match `.go-arch-lint.yml` exactly — `go-arch-lint check` is the ground truth.
- **Mutex inventories are current.** If a package gains or drops a mutex, or changes what it protects / whether it is held during IO, update that section's concurrency model.
- **Migrations.** New files under `migrations/` that change persisted state should be reflected in the affected package's State-ownership table.

Tooling: run `/pilat:arch-sync` to detect drift between this document and the code automatically.

---

# Package Internals

Per-package internals follow. Each section is self-contained: state ownership, concurrency model, ordering constraints, anti-patterns, and contracts for that package. The system-wide rules above take precedence where they overlap.

<a id="pkg-daemon"></a>

## `internal/daemon`

_Runtime coordinator: session lifecycle, persistence, private in-process manager contract, PubSub, admission control, subagent orchestration._


> **Cross-cutting insight**: Throughout coagent, SQLite is the source of truth for all persistent state. In-memory structures (maps, atomics, channels) are ephemeral coordination primitives that exist only during active processing. When DB and memory disagree, DB wins. This pattern originates in the daemon package and is assumed by all downstream packages.

### Purpose

The daemon package is coagent's runtime coordinator. It manages session creation, durable input admission, execution, parking (`/stop`), killing, clearing, resumption, and notifications through PubSub. Built-in managers call it **in-process** through the private `Controller` contract; normal messages have one transport (`session_inbox`) regardless of whether a runner is active.

It sits between the private manager contract and the session/agent execution layers, owning the concurrency model that determines how many sessions run simultaneously and how they receive input. It receives `sessionstore.OrchestrationStore` and `sessionstore.InboxStore` as separate capabilities; live session/message persistence is delegated through `sessionstore.RuntimeStore` passed via the factory. The daemon's own `Store` handles project identity, and `LinkStore` handles the durable subagent-completion ledger.

### Key types

| Type | Visibility | Role | Owns |
|------|-----------|------|------|
| `Service` | interface | Complete daemon implementation contract. The composition root immediately projects it into `schedule.SessionSender`, `applyVerdictSender` and `secretRequestResolver`; only the built-in manager adapter receives the complete surface | -- |
| `svc` | private struct | `Service` implementation -- the session manager (also implements the private `spawner` interface for subagent tools) | `loops` map, `mu`, `admit` (*admissionCtl), `queue`/`queueMu` (background-child FIFO), `pubsub`, `childCache`/`childMu` (publish-gate parent verdicts), `store`, `sessionStore` (`sessionstore.OrchestrationStore`), `inboxStore` (`sessionstore.InboxStore`), `links` (`LinkStore`), `factory`, `scheduleSvc`, `defaultModelFn`, `subagentModel`, `modelCatalog`, `shuttingDown` |
| `Store` | interface (4 methods) | Context-aware project identity persistence: GetOrCreateProject, GetProjectWorkDir, GetProjectName, ListProjects (id/name/work_dir of all projects; root-prefix filtering is done by the caller in Go, never SQL LIKE) | -- |
| `store` | private struct | `Store` implementation over `*sql.DB` | `db` |
| `LinkStore` | interface | Durable subagent-link reads and single-table lifecycle writes. Cross-table activation finalization/re-arm belongs to `sessionstore.OrchestrationStore`, which owns the participating SQLite transaction | -- |
| `linkStore` | private struct | `LinkStore` implementation over `*sql.DB` | `db` |
| `SubagentLink` | exported struct | A durable parent→child row: ParentID, ChildID, TaskCallID, Blocking, Depth, typed `LinkState`, DeliveredAt, DeliveredMsgID, TimeoutSec, CreatedAt, Result, typed `LinkOutcome`. `Terminal()` reports completed/error/killed | -- |
| `controller` | private struct | Implements `controllerapi.Controller` (the interface + its DTOs live in `controllerapi`, not here); `NewController` returns the interface type | `svc`, `cfg`, `cache` (`loader.MarketplaceCache`), `schedule` (`schedule.Service`, for the read-only `ListSchedules`) |
| `runner` | private struct | Per-session goroutine state. Ephemeral -- exists only while runner goroutine is alive | `service` pointer, typed exact-protocol `inputs` queue, `cancel` func, `done` channel, `workDir`, `projectID`, `kind`, `parentID`, first-iteration `hasRun` guard |
| `NotificationSource` / `pubSub` | exported subscription interface / private struct | Fan-out notification source; only `svc.publish` can reach the private non-blocking publish method | `subs` map, `globals` slice |
| `sessionInput` | private sealed interface | Daemon-internal async transcript protocol. Its variants are exact pending-call result, blocking/background child completion, normal schedule tick and fresh schedule task; impossible optional-field combinations are not representable | -- |
| `queuedSessionInput` | private sealed interface | Delivery policy around a `sessionInput`: child completions are async because `subagent_links` is their retry ledger; schedule/exact-result producers await actual transcript acceptance | -- |

### File map

| File | Responsibility |
|------|---------------|
| `manager.go` | `Service`/`svc`, durable message enqueue, stop/kill/clear/model/attributes/shutdown, typed exact-result/event routing, `NotifySession`, schedule cleanup |
| `controller.go` | `controller` impl of `controllerapi.Controller` -- the in-process API every manager calls; `NewController(svc, cfg, cache, scheduleSvc) controllerapi.Controller` takes a `loader.MarketplaceCache` so `ListSkills` can assemble marketplace plus project skills, and a `schedule.Service` for the read-only `ListSchedules` (maps `schedule.Entry` → `controllerapi.ScheduleInfo`, stripping `CRON_TZ=` for display). `ListSkills` filters only user visibility. The interface itself and all request/response DTOs live in `controllerapi`, not here |
| `controller_project.go` | `controller.CreateProject` (sanitize name → mkdir under root → `GetOrCreateProject`, idempotent) and `controller.ListRecentProjects` for the `/new` folder-project flow; `resolveProjectsRoot` (config `projects_root` or `~/.coagent/projects`, `~`-expanded + absolutized) and `sanitizeProjectName` (blacklist, 64-**rune** cap so cyrillic passes). mkdir happens daemon-side so managers never touch the filesystem |
| `project.go` | `svc.ListRecentProjects` (`RecentProject` type + `sortRecentProjects`): lists only direct children of root, ranks by newest non-killed top-level session activity (`sessionstore.LatestActivityByProject`), no-session projects sort first, ties by id desc |
| `linkstore.go` | `LinkStore` interface, `linkStore` struct, `NewLinkStore(db)`, `SubagentLink`, `LinkState*`/`LinkOutcomeIncomplete` constants -- the daemon-owned durable subagent ledger (moved out of `sessionstore.Store`) |
| `session_input.go` | Sealed `sessionInput` variants and their validation; `asyncSessionInput`/`awaitedSessionInput` delivery wrappers; resolver-vs-standalone classification and sleep-interruption reasons |
| `runner.go` | `runner` exact-protocol queue, `runSession`, `ensureRunner`, admission/timeout classification, typed event injection, producer-ledger merge, waiting projection, tool registration and error handling. Suspended runs exit; pending normal input restarts only when causally runnable |
| `input_recovery.go` | Durable-input runnability: pending inbox classification around external calls plus first-run inference of an active accepted turn from its persisted `accepted_message_id` identity |
| `input_boundary.go` | `durableInputBoundary`, the session-facing adapter that peeks/promotes/rejects/handles FIFO input and allows normal input to interrupt only sleep |
| `pending_runner.go` | best-effort root admission queue; the durable recoverable-input selector rebuilds runnable work after restart |
| `stop.go` | persisted tree discovery/queue removal for parking; transcript settlement stays runner-owned |
| `admission.go` | `admissionCtl` (kind-aware non-blocking admission: `tryAdmit`/`release`/`canAdmitChild`, caps `maxTotalSlots`/`maxChildSlots`/`maxInFlightPerParent`/`maxSubagentDepth`, atomic gauges), `slotKind` constants, `errNoCapacity` sentinel (the only `ensureRunner` failure that re-parks a child), `svc.enqueueChild`/`svc.drainQueue` (background-child FIFO, `childTerminated` guard skips a cascade-killed parked child and defers on an unreadable one) |
| `spawner.go` | private `spawner` interface impl on `*svc`: `Spawn` (creates child session + durable `subagent_links` row, depth cap, child-model resolution, detached-ctx child start, returns child id immediately), `Result` (child snapshot), `SendToChild` (re-arm idle/terminal child), `LinkPending` (resume idempotency check), `childSnapshot`/`childDepth`/`resolveChildModel`/`resolveChildEffort` (settles the child's reasoning level against the child model via `session.ResolveReasoningLevel`; an explicitly requested level the model rejects fails the spawn) |
| `compaction_defer.go` | `deferAnnouncements` — in-memory per-session ledger carrying the compaction-deferral-notice verdict across session rebuilds (`RunResult` → here → `CreateOptions`); forgets a session when its episode ends |
| `subagent.go` | private `spawnRequest`, `childResult`, `subagentInfo`, `modelInfo` data types, the `spawner` interface itself, and `formatChildResult` (the single shared completion formatter) |
| `mcp_tools.go` | The five MCP registry tools (`mcp_add`/`mcp_remove`/`mcp_enable`/`mcp_disable`/`mcp_list`), their shared `mcpDeps` (store + pool + project id), `mcpScope`, and the parameter docs. Mutating results all state that the change takes effect from the next run. `remove`/`disable` call `mcp.Pool.Evict` after the DB write |
| `mcp_tools_schema.go` | Their shared plumbing: `evict`, `scopeOf`/`parseNameScope`, `nameScopeSchema`, and `writeServerSection` — the `mcp_list` renderer that prints env **keys** only, never values |
| `task.go` | `taskTool` (`tool.Tool` impl, ID `tool.IDTask`) -- the `task` tool: `executeBackground`/`executeBlocking`, routes to `spawner.Spawn` |
| `subagent_result.go` | `getSubagentResultTool` -- diagnostic snapshot of a background child; model-facing contract explicitly forbids polling/wait loops |
| `subagent_send.go` | `sendToSubagentTool` -- the `send_to_subagent` tool, re-engages a background child via `spawner.SendToChild` |
| `completion.go` | child completion delivery: startup sweep, link-first terminalization, typed blocking/background routing, `injectBlockingCompletion` (must fill the exact linked pending task call), `injectBackgroundCompletion` (standalone synthetic pair only when no external call is pending), shared one-tx `persistCompletion`, `injectOwedCompletions` (drains terminal undelivered links after an exact result), result formatting, cascade kill and child resume |
| `finalize.go` | ledger-failure helpers shared by the teardown paths: `markLinkTerminalRetrying` (bounded `linkTerminalAttempts`/`linkTerminalBackoff` retry of `MarkLinkTerminal`, uninterruptible because callers pass a `WithoutCancel` ctx, plus an optional deadline -- `cascadeRetryBudget` is one budget for a whole cascade kill, not per node), `notifyChildFailure` (publishes to the **parent's** topic -- a child that never reached `announceSession` has none), `reportTimeoutUnresolved` (silent when the loop ctx is already cancelled) |
| `orphan_calls.go` | startup PASS 0: `resolveOrphanedCalls` (per-session comparison of the name-keyed transcript view against the merged producer ledgers), `orphanSweepCandidate` (only active/suspended/error, never killed), `closeOrphanedCalls` (adopts the call into the staged ledger, opens the session without a runner, resolves, un-adopts), `orphanedCallNotice` (the cancellation text), `unresolvedStoredExternalCalls` and `storedExternalCalls` (the durable name-keyed pending set, shared with `/stop`'s settle) |
| `transcript.go` | child-transcript scan helpers used by `deriveChildOutcome`: `lastStoredAssistantText` (backwards scan for the displayable result) and `lastStoredMessageIsFinalAnswer` (strict last-message check for completed-vs-incomplete) |
| `store.go` | `Store` interface, `store` struct, `NewStore()`, `ProjectRow`, project upsert/lookup/list keyed on `work_dir` |
| `pubsub.go` | `PubSub`, per-session and global fan-out; `Publish` takes a `sessionevent.Notification`, global subscribers receive `controllerapi.SessionNotification` (session ID + notification) |
| `publish.go` | `svc.publish` -- the single gate every production notification passes through: drops child-session events, `lookupChild`/`cacheChild` back the lazy `ParentID != 0` cache. Own file so the semgrep exclusion covers nothing else |

### State ownership

#### Persistent (SQLite -- source of truth)

| State | Table | Written by | Read by |
|-------|-------|-----------|---------|
| Project identity | `projects` (unique on `work_dir`) | `store.GetOrCreateProject` | Manager, `Controller` |
| Normal input acceptance | `session_inbox` FIFO (`pending` → `accepted`/`handled`/`rejected`/`cancelled`) | `sessionstore.InboxStore`; first promotion inserts the user transcript row and marks the session active in the same transaction | session `InputBoundary`, startup recoverable-input sweep, `/stop` cancellation |
| Scheduled occurrence delivery | `session_deliveries` identity/fingerprint ledger + transcript mutation | `sessionstore.ScheduledDeliveryStore`; claim and mutation are one transaction | schedule retry/ack, daemon applied receipt, session idempotent injection/reset |
| Subagent links ("completion owed") | `subagent_links` | initial row via `CreateSubagentWithLink`; simple lifecycle writes via `LinkStore`; cross-table activation finalization/re-arm and completion delivery via `sessionstore.OrchestrationStore` | admission, finalization, completion injection, waiting projection, stop/kill, startup sweep |

One `sessionstore.Store` implementation is projected as `RuntimeStore` for sessions, `ScheduledDeliveryStore` for exact-once scheduled transcript mutations, `InboxStore` for durable normal input, and `OrchestrationStore` for daemon lifecycle. `sessionstore` owns every transaction that spans its tables (`sessions`, `messages`, `session_inbox`, `session_deliveries`, and link-adjacent cross-table transitions); `daemon.LinkStore` owns independent link reads/writes.

#### Ephemeral (in-memory -- coordination only)

| State | Location | Protected by | Lifecycle |
|-------|----------|-------------|-----------|
| Active runners | `svc.loops` (`map[int64]*runner`) | `svc.mu` | Created in `ensureRunner`, deleted in `runSession` defer |
| Admission control | `svc.admit` (`*admissionCtl`) | `admissionCtl.mu` + atomic gauges | Slot reserved (non-blocking) in `ensureRunner` via `tryAdmit`, freed in `runSession` defer via `release`. Caps: `maxTotalSlots=16`, `maxChildSlots=12` (< total, so a completing child can always re-admit its suspended parent), `maxInFlightPerParent=8`, `maxSubagentDepth=3` |
| Background-child queue | `svc.queue` (`[]queuedChild`) | `svc.queueMu` | FIFO of background children that could not be admitted immediately; `enqueueChild` parks one, `drainQueue` (after every slot release) starts one whose parent now has capacity. The child's `subagent_links` row is already persisted (state `spawned`) before admission, so the restart sweep re-runs it on crash -- this map is only the in-memory ordering cache |
| Root admission queue | `svc.pendingRunners` | `svc.pendingMu` | Best-effort cache for roots whose durable input was accepted while all slots were occupied; startup rebuilds runnable obligations from pending inbox rows and active sessions backed by an accepted-message identity |
| Shutdown flag | `svc.shuttingDown` (`atomic.Bool`) | Atomic | Set in `Shutdown`; authoritative shutdown signal read by the `runSession` defer (not `ctx.Err()`) |
| Typed session-input queue | `runner.inputs` (`[]queuedSessionInput`) | `runner.inputMu` | Appended by manager routing, causally ordered and drained at the start of each loop iteration |
| Live session reference | `runner.service` (`session.Service`) | `runner.svcMu` | Set when session created, **nil between loop iterations** |
| Runner done signal | `runner.done` (`chan struct{}`) | Channel semantics | Closed when `runSession` goroutine exits; waited on by `stop()` |
| Publish-gate child verdicts | `svc.childCache` (`map[int64]bool`) | `svc.childMu` | Filled lazily on the first publish for a session (`ParentID != 0`), never invalidated -- `ParentID` is immutable and a child is never promoted to root. A failed lookup is not cached, so the fail-open answer is retried. Cleared only by restart |
| PubSub subscriptions | `pubSub.subs`/`globals` | `pubSub.mu` (RWMutex) | Subscribe/Unsubscribe per consumer -- `Controller.SubscribeAll()` is the only production caller (a manager, e.g. Telegram, reads the channel in-process) |

### Concurrency model

#### Goroutines

1. **Runner** (per active session) -- spawned by `ensureRunner`. It constructs a session with a durable `InputBoundary`; suspended runs exit and release admission instead of polling.
2. **Shutdown waiter** -- spawned by `Shutdown`; stops active runners in parallel with a timeout.
3. **Startup sweep** -- after synchronously finishing any persisted `stopping` operation, resumes in-flight children, re-delivers owed completions, and admits causally-runnable pending or active accepted input.

#### Mutex inventory

| Mutex | Protects | Held during IO? |
|-------|----------|----------------|
| `svc.mu` | `loops` map only | **No** -- released before any IO, DB, or admission call |
| `svc.queueMu` | `queue` slice (background-child FIFO) | **No** -- released before `ensureRunner` / `canAdmitChild` |
| `svc.pendingMu` | pending root admission cache | **No** |
| `svc.childMu` | `childCache` map only | **No** -- never held across the `GetSession` lookup or the `pubsub.Publish` fan-out. Deliberately not `svc.mu`, which is held around runner starts and would put the publish path behind them |
| `admissionCtl.mu` | `running`/`runningChild`/`perParent` counters | **No** -- short critical section only |
| `pubSub.mu` (RWMutex) | `subs`, `globals` | **No** -- copies subscriber slices under RLock; fan-out and warning logs happen after unlock. Subscribe/unsubscribe take the write lock |
| `runner.svcMu` | `service` pointer only | **No** -- brief acquire to read/write pointer |
| `runner.inputMu` | `inputs` slice | **No** |

#### Cross-goroutine communication

- **PubSub channels** (buffer 64): Per-session and global. Non-blocking publish -- **silently dropped if full**.
- **runner.done** (chan struct{}): Closed when `runSession` exits. `stop()` waits on it after calling `cancel()`.

### Construction and lifecycle (manual composition root)

There is no DI framework and no lifecycle hooks registered inside constructors. `cmd/coagent/main.go` calls each of the methods below explicitly, in order -- see [Initialization sequence](#initialization-sequence) for the full picture across all packages.

#### Manager (`svc`)

`New(factory session.Factory, store Store, sessionStore sessionstore.OrchestrationStore, inboxStore sessionstore.InboxStore, links LinkStore, scheduleSvc schedule.Service, cfg *config.Config, ...) Service`

- Delegates to `newSvc()` which creates the struct and runs `sessionStore.KillTerminatingSessions()` cleanup.
- `defaultModelFn` is taken from `cfg.DefaultModel` (a method on `*config.Config`); `subagentModel` and `modelCatalog` from `cfg.SubagentModel`/`cfg.UnifiedConfig.Models`.
- Creates an `admissionCtl`.
- Registers no hooks. `main.go` calls `daemonSvc.Start(ctx)` right after construction (spawns the detached restart-recovery `sweep` goroutine) and registers `func(ctx) error { daemonSvc.Shutdown(30 * time.Second); return nil }` as its own named stop closure.

`newSvc(factory, store, sessionStore, inboxStore, links, scheduleSvc, defaultModelFn)` -- shared by `New` and by tests (tests call it directly to skip the model-catalog/subagent-model setup `New` does).

#### Lifecycle sequence

```
Construction (main.go, in order):
  1. NewStore(db), sessionstore.NewStore(db), schedule.NewStore(db), memory.NewCuratedStore(db),
     NewLinkStore(db)         -- pure constructors, no hooks
  2. New(factory, ...)        -- creates svc (with admissionCtl)
  3. daemonSvc.Start(ctx)     -- spawns the detached restart-recovery sweep goroutine, returns immediately
  4. NewController(daemonSvc, cfg, cache, scheduleSvc)

Shutdown (reverse of the app-level onStop order -- see Shutdown sequence):
  1. managers.Runtime.Stop, schedule.Executor.Stop -- run first (registered after daemon)
  2. daemonSvc.Shutdown(30s)   -- stops all runners with timeout
```

### Data flow

#### Session creation (primary happy path)
```
Controller.CreateSession(data)   <- in-process call from a manager (e.g. managers/telegram)
  -> (optional) git worktree creation if data.UseWorktree set
  -> svc.GetOrCreateProject(ctx, workDir) -> projectID
  -> svc.Send(ctx, projectID, prompt, model, attrs)
    -> sessionStore.CreateSession -> sessionID
    -> sessionStore.EnqueueInput(prompt)          // commit before acknowledgement
    -> ensureRunner(ctx, sessionID, workDir, projectID, nil)
      -> slotInfo (parent) -> admit.tryAdmit (non-blocking)
         -> create runner -> store in loops -> go runSession
        -> load session record
        -> publish NotifySessionCreated (name, workDir, attrs)
        -> factory.Create -> session.Service
        -> runner.service = sess (under svcMu)
        -> registerScheduleTools / registerSubagentTools (both gated by AgentTypes().FilterTools)
        -> registerMCPTools (root sessions only — a subagent must not reshape its parent's toolset)
        -> factory.Create(... InputBoundary=durableInputBoundary)
        -> sess.RunDaemon(notifyCallback)
          -> settle previous assistant/tool state first
          -> boundary.Peek -> PrepareUserMessage -> PromoteInput
             (one tx with message insert, inbox acceptance, and active session status)
          -> run the next LLM turn
            -> notifyCallback -> svc.publish (drops child sessions) -> PubSub.Publish -> per-session/global subscriber channels
               (consumed in-process by whichever manager subscribed, e.g. Telegram; no WS broadcast)
  -> controller.publishInputReceived(sessionID, prompt, "user")
```

#### Filesystem browsing (Telegram directory picker)
```
Controller.ListDir(data)
  -> controller.listDir(data)
    -> favorites := cfg.UnifiedConfig.SpawnFavorites  (never nil -- [] when unset)
    -> data.Path == "" -> $HOME  (unconditional; favorites do not suppress the fallback)
    -> readSubdirs(path): visible directories only, newest mtime first
    -> Parent = filepath.Dir(path), empty when path == "/"
```

#### Normal session message (one durable path)
```
Controller.SendSessionMessage(data)
  -> svc.SendToSession(ctx, sessionID, message)
    -> sessionStore.EnqueueInput(message)       // durable FIFO, always first
    -> live runner consumes it at a causal boundary; idle root is admitted or queued
    -> pending non-sleep external call: message remains pending
    -> pending sleep only: exact sleep result is recorded, timer removed, then message promotes
  -> controller.publishInputReceived(sessionID, message, "user")
```

There is no running-vs-idle transport distinction and no steering channel. Admission
failure cannot lose an acknowledged message: roots enter the pending-runner cache and
the inbox row reconstructs that cache after restart.

#### Typed session-input delivery (schedule/sleep wake-ups, subagent completions)
```
schedule.Executor / sleep timer / child finalizeChild fires
  -> exact DeliverPendingCallResult / DeliverScheduleTick / DeliverFreshSchedule
     or typed deliverCompletionToParent
    -> routeQueuedSessionInput(ctx, sessionID, input)
      -> appendIfLive(sessionID, input): under svc.mu, hit -> rs.appendSessionInput
      -> miss: GetSession (reject killed) -> resolve workDir -> appendIfLive re-check
      -> still miss: ensureRunner with inputs (lazily revives an idle session)
  -> runSession next iteration (prepareSessionInputs):
    -> rs.drainSessionInputs(); stable-partition exact resolvers first
    -> exact result: sess.ResolvePendingCall({ID, Name}, content)
    -> blocking completion: verify link + exact pending task call, then atomic delivery
    -> standalone child/schedule event: first resolve exact pending sleeps, then cancel timers
    -> if another external call remains: defer/retry; never append across it
    -> inject normal tick/background pair or ResetContextAndInjectOnce(fresh prompt)
```

#### External notification (NotifySession)
```
Any caller (e.g., schedule.Executor via SessionSender, tool, subagent)
  -> svc.NotifySession(sessionID, notification)
    -> pubsub.Publish(sessionID, notification)
      -> per-session subscribers + global subscribers (in-process channel sends only)
```

#### Session clear
```
Controller.ClearSession(data)
  -> svc.Clear(sessionID)
    -> read old session record (model, attrs, projectID)
    -> sessionStore.UpdateSessionStatus(sessionID, "terminating")
    -> sessionStore.CreateSession(replacement) -> newRec
    -> pubsub.Publish NotifySessionCleared (old->new mapping)
    -> Kill(sessionID) -> stop runner, markSessionKilled, removeSchedules
  -> return newRec.ID
```

#### Kill session
```
svc.Kill(sessionID)   <- from Controller.KillSession, or cascadeKillChildren
  -> if runner active: pubsub notify "Stopping session...", rs.stop() (cancel + wait on done)
  -> sessionStore.GetSession -> verify not already killed
  -> sessionStore.MarkSessionKilled
  -> removeSchedules(sessionID) -> scheduleSvc.RemoveAllForSession
       (separate call, NOT part of MarkSessionKilled's own transaction)
  -> cascadeKillChildren(sessionID, 0) -> recursively kill every in-flight
       non-terminal descendant (blocking AND background, depth-bounded);
       completed-but-undelivered descendants are skipped (result preserved)
  -> pubsub.Publish NotifyStateChanged(idle, "killed")
```

### Runner lifecycle

```
ensureRunner -> slotInfo classifies (parent vs child) -> admit.tryAdmit (non-blocking)
             -> runner created, registered in loops, goroutine spawned
  |
  v
runSession entry: teardown defer registered FIRST, then if rs.kind == slotChild ->
  applyChildTimeout (blocking children only; inherits the task `timeout` param, default
  300s; background children + parents run untimed). On a ledger read failure it sets
  `errored` and returns early THROUGH the teardown defer -- see the ordering note below.
  |
  v
runSession loop:
  +------------------------------------------------------+
  | load session record from sessionStore                 |
  | (first iteration: publish NotifySessionCreated)       |
  | createOrResumeSession -> sess                         |
  | load producer ledgers -> staged external calls         |
  | drain exact-protocol inputs                            |
  | durable InputBoundary remains session-owned            |
  | if no inbox row, typed input, or pending work:         |
  |   before its first execution only, infer an unfinished |
  |   promoted user turn from durable canonical state;     |
  |   otherwise sess.Close, notify idle, return            |
  | runner.service = sess (under svcMu)                    |
  | registerScheduleTools                                  |
  | executeSession -> sess.RunDaemon(notify)               |
  | runner.service = nil (under svcMu) <- KEY MOMENT       |
  |                                                        |
  | if runErr != nil -> sess.Close, handleRunError,        |
  |   notify idle, return                                  |
  | sess.Close                                             |
  | suspended -> publish ledger-projected wait and exit    |
  | otherwise loop only while immediate work remains       |
  +------------------------------------------------------+
  |
  v
defer: recover panic (-> errored) -> read shuttingDown (authoritative, not ctx.Err())
  -> delete(loops, sessionID) -> admit.release(kind, parentID)
  -> finalizeChild(detached ctx, sessionID, shuttingDown, errored)  <- subagent only; no-op on shutdown
  -> drainSessionInputs (leftover) -> close(done)
  -> if normal exit: re-route exact-protocol leftovers + drain queues
  -> if stopped/killed: complete awaited typed senders with an error
  -> timeoutCancel() LAST (blocking children only)
```

### Subagent orchestration

Subagents are first-class daemon-driven sessions, not in-process forks. A parent session delegates work via the `task` tool — which is itself a daemon type (`daemon/task.go`), registered onto the session's live registry from outside — and that tool calls `svc.Spawn` through the daemon's own private `spawner` interface. The child is an ordinary session row with its own runner, governed by the same admission/runner machinery as any other session and distinguished only by its durable `subagent_links` row.

#### The durable link is the "completion owed" ledger

Every spawn inserts a `subagent_links` row (`parent_id`, `child_id`, `task_call_id`, `blocking`, `depth`, `state`, `delivered_at`). The link state -- not memory -- is the source of truth:

- `slotInfo` reads it to classify a session as a child (and recover its `parent_id` + `blocking` flag) for admission accounting.
- `finalizeChild` marks it terminal (`completed`/`error`/`killed`); typed blocking/background injection stamps `delivered_at` atomically with the parent transcript rows.
- A background child parked in the in-memory FIFO keeps its already-persisted `state='spawned'` row (the row is inserted before admission); the startup sweep rebuilds in-flight state purely from these rows. If the daemon crashes between any two steps, the link tells the sweep exactly what is still owed.

#### Spawn flow: blocking suspend vs. background

```
parent's task tool -> taskTool.validateParams rejects an unknown agent type up front
parent's task tool -> svc.Spawn(spawnRequest)
  -> createChildSession -- agent type already validated by the caller
  -> depth = childDepth(parentID); reject if depth >= maxSubagentDepth
  -> resolveChildModel: req -> agent config -> daemon subagentModel -> parent model
  -> CreateSubagentWithLink (one tx: child session + state=spawned link + initial inbox row) -> childID
  -> ensureRunner(context.WithoutCancel(ctx), childID, ...)   <- DETACHED ctx: the
       parent's tool-call ctx (which may time out) never kills the child
  -> return childID immediately (never blocks)
```

- **Blocking** spawn: the `task` tool requires a non-empty call ID, returns `ErrSuspend`, and the parent's loop exits and **releases its run-slot**. A blocking child gets a wall-clock timeout (`applyChildTimeout`, inherits the `task` `timeout`, default 300s). On resume, the completion verifies the durable link and fills that exact pending `task_call_id`; it never degrades into a background event.
- **Background** spawn: the parent keeps running; the child is admitted if it fits, otherwise `enqueueChild` parks it. The completion is later added as a synthetic `subagent_event` tool pair (keyed by `child_id`).

Child completion is the join/wake primitive; `sleep` is never a subagent wait
primitive. `session.executeToolCallsInternal` preflights a parallel tool batch and
rejects `sleep` before side effects when the same batch contains either `task` or
`send_to_subagent`. If a model asks to sleep on a later iteration while any child
completion is still owed, daemon's `subagentWaitGuard` rejects it before a timer
is persisted. The parent may instead finish its turn or continue independent
work; the durable child link wakes it automatically on completion.

#### Admission caps and the FIFO queue

`admissionCtl` is kind-aware and **non-blocking** (`tryAdmit` returns a bool, never waits):

- `maxTotalSlots=16` caps all loops; `maxChildSlots=12` caps subagents **below** the total; `maxInFlightPerParent=8` bounds fan-out; `maxSubagentDepth=3` bounds nesting.
- On a child admit-fail, `ensureRunner` **queues** a background child (`enqueueChild`) or **errors** a blocking one (`"session capacity reached"` -- surfaced to the model as a tool_result so it degrades gracefully).
- `drainQueue` runs after every slot release (in the runSession defer) and starts one queued child whose parent now has capacity (`canAdmitChild`); `ensureRunner` re-checks admission, and the child is re-parked **only** on the `errNoCapacity` sentinel. Any other `ensureRunner` error (e.g. an unreadable ledger) is logged at `Error` and the entry is dropped -- blind re-parking on a persistent failure would spin the queue.

#### Suspended-parent-holds-zero-slots (the key invariant)

A suspended (blocking) parent occupies **no** run-slot. Because children are capped strictly below the total, there is always a free slot a completing child can use to re-admit its parent. This is what kills the priority-inversion deadlock: a parent waiting on a child can never starve the child, and a completing child can always wake the parent it was blocking.

#### Explicit result + completed/incomplete outcome

At terminalization `finalizeChild` derives two values from the committed transcript. `TryFinalizeSubagentActivation` conditionally writes them only when no follow-up is pending and `/stop` has not moved the child to `stopping`; SQLite serializes those competing writes without a process-local lifecycle mutex:

- **`result`** -- the child's final answer text (backwards-scan for the last text-only assistant message), or a short context note (`ended without a final answer after N iterations`) when there is none.
- **`outcome`** -- the richer parent-facing signal, derived from `(errored, hasFinalAnswer)`: `completed` when the **last** assistant message is text-only with content; `error` when `errored` (panic or blocking-timeout); `incomplete` otherwise (hit the iteration cap, stopped mid-tool, or returned consecutive empties). The kill path stores `killed`. `outcome` is independent of the lifecycle `state` column -- e.g. a max-iterations child is `state=error` but `outcome=incomplete`.

Both the auto-delivered completion (`completionContent`) and the `get_subagent_result` tool read these columns and format through the single shared private `formatChildResult` helper (in `daemon/subagent.go`), so they surface an **identical** string. The parent thus never sees a silent `completed`/`(no output)` masquerading as success; a child that stopped without a final answer reports `incomplete` explicitly. A blank `outcome` (a link terminalized by an older binary and redelivered post-upgrade) falls back to a neutral `finished` label.

#### At-least-once, idempotent completion delivery (single transaction)

`finalizeChild` (in the runSession defer) marks the child terminal and calls `deliverCompletionToParent`, which chooses a typed blocking or background input from the durable link and routes it through `routeQueuedSessionInput` -- reviving an idle parent if needed. Blocking delivery validates both `child_id` and the linked `task_call_id`; background delivery is a standalone `subagent_event` and cannot cross a pending external call. Both commit **exactly once** through `DeliverCompletionAtomic`: in one transaction it CAS-stamps `delivered_at`, inserts the completion message(s), and stamps `delivered_msg_id`. A crash commits both or neither; redelivery loses the CAS and inserts nothing. On a winning CAS the persisted messages are appended to the live in-memory transcript. A killed parent rejects the completion (orphan policy), while a temporarily blocked background completion remains owed in `subagent_links` and is drained after the exact call result.

#### Cascade-kill and restart recovery

- **`Kill`** calls `cascadeKillChildren`: every in-flight **non-terminal** descendant is recursively killed (depth-bounded), **blocking and background alike**. A deliberate tree teardown leaves no live receiver, so a surviving background descendant would keep consuming a slot and writing files while reporting to nobody. Terminal links (a completed-but-undelivered child included) are skipped so their stored `result`/`outcome` survive and they are not mislabelled `killed`; each non-terminal descendant torn down emits one `cascade_killed_descendant` WARN. `killSubagent` marks the child link terminal **before** stopping its runner, so the child's own teardown sees a terminal link and no-ops; if that mark cannot be written it skips `UpdateSessionStatus`/`MarkSessionKilled` so the child stays sweep-recoverable, and still stops the runner. The whole walk shares one `cascadeRetryBudget` -- it is sequential and `Kill` waits on it synchronously, so a per-node budget would multiply by the size of the tree. A child cascade-killed while still parked in the FIFO is guarded at `drainQueue` (`childTerminated`) so a later drain never launches it. `childTerminated` has three outcomes, not two: terminated, not-terminated, and **unreadable** -- on the third `drainQueue` re-parks the entry and returns *without* recursing, because the `true` branch recurses and would otherwise flush the whole queue of live children on one stuck read.
- **`sweep`** (`svc.Start(ctx)`, which `main.go` calls explicitly right after `daemon.New`) recovers after a restart: PASS 0 (`resolveOrphanedCalls`) closes external calls whose producer did not survive the previous image -- it must precede every resume, because such a call blocks its session's next request. `Start` runs PASS 0 **inline and spawns only the resumes** (`resumeAfterRestart`): managers and the schedule executor start the moment `Start` returns, and a runner either of them opens makes PASS 0 skip that session for the rest of the boot (skipped sessions are logged as `orphan_sweep_skipped_running_session`, which is a broken ordering contract, not a benign race). Its cost is bounded at one transcript read per active/suspended/error session. PASS 1 resumes children still in-flight (`ListRunningChildLinks` -> `resumeChild`); PASS 2 re-delivers terminal-but-undelivered completions (`ListUndeliveredParentLinks`) to live/idle parents. This is why `finalizeChild` is a no-op during shutdown -- the durable link guarantees the sweep re-delivers. PASS 3 (`resumeSessionsWithRecoverableInput`) resumes roots whose normal input is either still pending or belongs to an active turn backed by an accepted inbox row with `accepted_message_id`. The accepted identity survives every later transcript shape -- user-only, assistant tool call, tool results, compaction, or a final assistant awaiting its status checkpoint -- while excluding a lone AGENTS.md header and handled/rejected commands. Every newly created runner, whether admitted immediately, delayed in a queue, or won through a startup/controller dedup race, may derive that same durable obligation only before its first execution (`runner.hasRun == false`); after one execution the ordinary idle guard applies. This also lets children resumed by PASS 1 recover the same crash window without transporting volatile recovery metadata through admission queues. Every pass always runs (there is no early exit), and the closing log line is the assertion surface: `sweep_done{resumed,redelivered,input_resumed}` when every query succeeded, `sweep_incomplete{...,running_failed,undelivered_failed,input_failed}` when one failed -- a failed pass iterates an empty slice, so reporting `sweep_done resumed=0` would dress a failed recovery as a clean one.

- **`Stop`** is non-destructive tree parking. It writes `stopping` for the root and every descendant discovered from `sessions.parent_id`, stops active/queued runners, cancels pending inbox input and exact pending-sleep rows, resolves outstanding calls as stopped, makes stopped child links explicitly resumable, then writes `stopped`. `settleStoppedCalls` takes its list from the **durable transcript** (`storedExternalCalls`) and adopts it into the staged ledger first, exactly like PASS 0: the second phase may run in a process image that never staged the call (a `request_secret` records nothing else), and a stopped session is resumable, so leaving its `tool_use` dangling bricks it. Recurring schedules, standalone one-shot scheduled input, and transcripts survive. Startup synchronously finishes trees stranded in `stopping` before the sweep; a normal root message resumes only the root, while a child requires `send_to_subagent`.

### Tensions

1. **Normal and exact-protocol inputs have different durability.** Normal messages are already acknowledged by the `session_inbox` commit and need no in-memory wake signal; typed external results use the runner queue and may await transcript acceptance. A suspended runner exits, so ledger producers lazily re-admit it instead of keeping a polling goroutine alive.

2. **`runner.service` is nil between loop iterations.** External callers (`SetModel`) acquire `svcMu`, check for nil, and skip the in-memory update if nil. Changes take effect via the DB record at the next iteration. "Success" from these methods means "persisted, will apply eventually" -- not "applied now." `SetModel` validates before it persists — the live session's refusal (or, for an idle session, the configured-model catalog) comes back as the error, so a session record never names a model no client can be built for. It also settles the effort level against the model catalog (`session.ResolveReasoningLevel`) before persisting, so the record's `reasoning_level` is always what a run will actually request.

3. **`SubscribeAll` fan-out is unfiltered.** Any consumer calling `NotificationSource.SubscribeAll()` (e.g. a manager) receives every root session's notifications, not just the ones it cares about (child sessions never reach the hub -- `svc.publish` drops them). There's no per-consumer session filtering at the hub level -- each subscriber is responsible for ignoring notifications it doesn't own.

4. **PubSub drops notifications silently for slow subscribers.** The non-blocking send with `select/default` means a subscriber that falls behind by 64 messages starts losing notifications with only a log warning. There's no backpressure or replay mechanism.


5. **Clear publishes to old session ID before killing.** The `session.cleared` notification goes to the old session's PubSub subscribers. The controller/manager must be subscribed to the old session to learn about the new one. This is intentional -- it doesn't know the new ID yet.

6. **Blocking and background children fail differently under load.** A blocking child that cannot be admitted errors immediately (`"session capacity reached"`) and the parent gets that text as a tool_result -- it does *not* queue, because the parent is already suspended waiting on it. A background child queues and runs later. So the same global cap manifests as a hard error for one delegation mode and a soft delay for the other; the model sees no signal that a background child is merely parked.

7. **(Resolved) Cascade-kill now reaps background descendants too.** A deliberate tree teardown (`Kill`/`Clear`) recursively kills **every non-terminal** descendant -- blocking and background -- so no background work outlives a torn-down tree, keeps writing files, and reports to a dead session. This is distinct from the **idle**-parent path, which still survives (a background child completing against a merely-idle parent revives it). Completed-but-undelivered descendants are skipped (their `result`/`outcome` survive); queued descendants killed before they ran are guarded at `drainQueue`.

8. **The in-memory background queue is a cache, not the ledger.** `svc.queue` can be lost on crash; recovery relies on the child's persisted `subagent_links` row (state `spawned`, written by `Spawn` before admission) being re-read by the sweep. The queue and the link state can diverge briefly (e.g. a child enqueued in memory before its row is observed), and `drainQueue`/`ensureRunner` re-check admission to absorb the race rather than trusting the in-memory slot count alone.

9. **Final-response publication has no durable acknowledgement.** The assistant row is committed before its PubSub notification, and the `completed` checkpoint follows both. Recovery can therefore settle an active turn whose final assistant is already stored without re-running the model or replaying a response, but it cannot distinguish a crash before publication from one after publication. The former may lose the controller-visible answer; replaying would duplicate the latter. Closing this window requires a durable controller-delivery/outbox protocol, not another transcript heuristic.

### Ordering constraints

- **Persist-before-notify**: DB write must complete before PubSub publish. (Why: DB is source of truth. If you notify first, clients see state that isn't committed. `send` follows this: store insert, then ensureRunner, then publish.)

- **Registry-delete-before-admission-release (runSession defer)**: `delete(s.loops, sessionID)` must happen before `s.admit.release(kind, parentID)`. (Why: if reversed, a new `ensureRunner` can win admission and find the old entry still in `loops`, incorrectly merging messages into the dying runner.)

- **Session.Close-before-idle-notify (runSession exit)**: `sess.Close()` before publishing `StateIdle`. (Why: subscribers reacting to "idle" may immediately call `SendToSession`, which starts a new runner. If the old session is still open, two `session.Service` instances would exist for the same session ID.)

- **`runner.service = nil` before loop-back/exit**: The runner clears the `service` field under `svcMu` after `executeSession` returns and before closing the session or checking the inbox. (Why: prevents external callers from accessing a stale session object that's being closed.)

- **`close(done)` after `finalizeChild`, before re-routing/queue drain (defer)**: The defer chain is: delete from loops, `admit.release`, `finalizeChild`, drain leftover typed inputs, `close(done)`, then re-route those inputs and `drainQueue`. `stop()` waits on `done`, so anything that depends on the runner being fully cleaned up sees a consistent state. `finalizeChild` runs before `close(done)` so a parent observing a child's `stop()` returning also sees the completion already in flight.

- **Teardown defer registered before `applyChildTimeout`; `timeoutCancel()` runs last inside it (runSession)**: `runSession` declares `var timeoutCancel context.CancelFunc`, registers the teardown defer, and only then calls `applyChildTimeout`. (Why, two hazards at once: an early `return` on the timeout-read error path must still run the full teardown, or `rs.done` is never closed and every `stop()` on that session hangs forever on `<-r.done`; and the cancel must fire *after* the teardown reads `ctx.Err()`, because a `cancel()` that ran first would make the teardown see a cancelled ctx and mark **every normally-finished blocking child** `LinkStateError`. A plain `defer cancel()` placed after the teardown defer would do exactly that -- LIFO runs it first. `blocking_test.go`'s completed-outcome assertion is the regression gate.)

- **Mark-terminal-before-stop (killSubagent)**: `links.MarkLinkTerminal(killed)` + `sessionStore.UpdateSessionStatus` + `sessionStore.MarkSessionKilled` run before `rs.stop()`. (Why: when the child's runner exits, its own `finalizeChild` reads the link, sees it already terminal, and no-ops -- instead of racing to deliver a stray completion to a parent that is itself being killed.)

- **CAS-and-insert in one transaction (typed completion -> DeliverCompletionAtomic)**: The `delivered_at` CAS and the completion-message insert(s) commit in a **single transaction** -- a crash commits both or neither. (Why: at-least-once, exactly-once-effective delivery with no crash window. `delivered_at` alone is the dedup; a redelivery loses the CAS and inserts nothing.)

- **Only the atomic CAS resolves a blocking `task` tool_use (load-bearing)**: A blocking completion is the *sole* writer of the `task` tool_use's result. `pendingExternalCallIDs` keeps `task` out of `repairTranscriptExcluding` stubbing, so transcript repair never fabricates a competing `tool` row. (Why: if repair could stub the pending `task`, two results would race; A1's exactly-once guarantee rests on the CAS being the only resolver.)

- **`managers.Runtime.Stop` before `svc.Shutdown` (main.go registration order)**: `main.go` registers the managers' stop closure after the daemon's, so shutdown (LIFO) stops managers first. This ensures no new in-process controller calls arrive while runners are being drained.

### Package anti-patterns

- **Don't hold `svc.mu` during store or session operations.** The manager explicitly releases `mu` before calling store methods or creating sessions. Holding `mu` during SQLite queries would block all session management (Send, Kill, List, SetModel) for the duration of the I/O.

- **Don't read `runner.service` without `svcMu`.** It's `nil` between loop iterations and set from the runner goroutine. Reading it directly from any other goroutine is a data race. Always: lock `svcMu`, copy the pointer, unlock, then use the copy.

- **Don't assume a single lock-check-unlock prevents duplicate runners.** `ensureRunner` uses a double-check pattern (check loops, unlock, `admit.tryAdmit`, re-lock, re-check, register). `tryAdmit` is non-blocking now, so the window between the two checks is narrower than it was under the blocking semaphore -- but it still exists (any work between unlock and re-lock), so the re-check is still required. Code that skips it will create duplicate runners for the same session (and the loser must `admit.release` the slot it reserved).

- **Don't send on PubSub subscriber channels under `PubSub.mu`.** Publish copies channel slices under RLock then sends outside the lock. Sending under the lock would risk deadlock if a subscriber's goroutine tries to Unsubscribe while the publisher holds the lock.

- **Don't construct `svc` without also calling `Start(ctx)` and registering `Shutdown` in production.** `New()` itself registers no hooks -- there is no lifecycle framework. `main.go` must call `daemonSvc.Start(ctx)` right after construction and register a stop closure that calls `daemonSvc.Shutdown(30 * time.Second)`. Using `newSvc()` directly (as tests do) skips both -- acceptable only in tests.

### Contracts

- **Admission-loops invariant**: Every entry in `svc.loops` corresponds to exactly one admitted slot. The `defer` in `runSession` that deletes from `loops` and calls `admit.release(kind, parentID)` must always execute as a pair, with the matching `kind`/`parentID` the slot was admitted under. (Why: breaking this leaks slots from the `running`/`runningChild`/`perParent` counters, eventually preventing any new session -- or any child of that parent -- from starting.)

- **One runner per session ID**: `ensureRunner` uses double-checked locking to guarantee at most one goroutine per session ID in `loops`. Violating this (e.g., skipping the re-check) would cause duplicate execution of the same session with split message delivery.

- **Runner self-cleanup is non-negotiable**: The `defer` block in `runSession` (delete from loops + `admit.release` + `finalizeChild` + close done + re-route/drainQueue + `timeoutCancel`) must never be bypassed -- including by an early `return` before the loop starts, which is why the defer is registered ahead of `applyChildTimeout`. (Why: orphaned entries in `loops` prevent session restart; leaked admission slots reduce the concurrency limit permanently; a skipped `finalizeChild` leaves a parent suspended forever on a completion that never arrives; unclosed `done` causes `stop()` to hang forever.)

- **`runner.service` validity window**: Only valid under `svcMu` and only during execution. External callers must acquire `svcMu` and check for nil before accessing the session. Between loop iterations, `service` is nil -- accessing it without the nil check causes a panic.

- **Inbox signal is edge-triggered, not level-triggered**: The buffered(1) channel coalesces multiple `Append` calls into a single wake-up. Consumers must always call `Drain()` after receiving a signal -- checking the signal alone is not sufficient to know how many messages are pending.

- **`stop()` is synchronous**: `runner.stop()` cancels the context and blocks until `runSession` exits (via `<-done`). This means `Shutdown` can guarantee all runners have exited when it returns within timeout. Callers of `stop()` must not hold `svc.mu` -- `runSession`'s defer acquires it.

- **`NotifySession` is fire-and-forget**: `NotifySession(sessionID, n)` delegates to `svc.publish` with no runner lookup. It does not wake dormant sessions or create runners. It's a pure notification passthrough for external callers (e.g., schedule executor) that already know the session exists.

- **Project identity is the workdir**: The `projects` table has a unique index on `work_dir`. `GetOrCreateProject` upserts on the absolute path and returns the existing row for a repeat call.

- **A child is admitted below the parent cap on purpose**: `maxChildSlots < maxTotalSlots`, and a **suspended parent holds zero slots**. Together these guarantee a free slot is always reachable to re-admit a completing child's parent, so a blocking parent can never deadlock its own child. Code that raises `maxChildSlots` to `maxTotalSlots`, or that keeps a slot reserved while a parent is suspended, reintroduces the priority-inversion deadlock.

- **A ledger read failure is never "no link" (fail-closed)**: Every read of `subagent_links` distinguishes three outcomes, not two: the row exists, the row does not exist, and the read failed. Collapsing the third into the second always picks the *weaker* behaviour -- "not a subagent", "not killed", "nothing to do" -- so an unreadable ledger silently drops the daemon's own limits or loses a completion. A read failure must therefore refuse the operation (`childDepth`, `slotInfo`, `applyChildTimeout`, `childTerminated`, typed completion injection return an `error`) or say so loudly where there is no caller to refuse (`finalizeChild`, `cascadeKillChildren`, `killSubagent`, `sweep`). The **`link == nil` branch must stay untouched** -- every root session takes it on the normal path.

- **Ledger-failure notifications address the parent, never the child**: A subscriber learns a session's topic from `announceSession`, which runs *inside* the runSession loop. A child that dies before the loop has no topic, so a publish to its id reaches nobody -- `notifyChildFailure` publishes to `link.ParentID`/`rs.parentID`. When the link itself could not be read the parent id is unknown, so that path logs at `Error` and publishes nothing; there is no fallback to invent.

- **The link is authoritative over memory**: A completion is owed iff a non-terminal `subagent_links` row says so; it is delivered iff `delivered_at != 0`. In-memory state (`loops`, the background-child queue) is a cache. On any restart the sweep rebuilds purely from links. Never treat the absence of an in-memory runner/queue entry as "nothing owed" -- check the link.

- **Spawn never blocks; capacity surfaces as a tool result**: `Spawn` returns the child id immediately and never suspends. A blocking child that cannot be admitted errors (`"session capacity reached"`), which reaches the model as a `task` tool_result so it degrades gracefully; a background child that cannot be admitted is queued, never dropped. Callers must not assume a successful `Spawn` means the child is already running.

- **One staged config apply per daemon, not per session**: the pending-apply marker, `config.yaml` and the restart an apply ends in are all process-global, so the slot that guards them is too. `ConfigApplier.ClaimApply` is the one gate; `svc.stageApply` takes it for a session tool and the bootstrap `set_provider` op takes it for the socket *before* writing the provider key (a refusal must leave no orphan credential on disk), and a refusal must reach the tool *before* it suspends — a staged change that never commits leaves its call with no producer. The slot comes back only through `ReleaseApply`, which `Apply` calls when the commit itself failed (no restart is coming, and the rejection is delivered in-process). A committed change keeps the slot for the rest of the process image: the restart is what clears it. Note `runStagedApply` runs after every session run, including one that ended because the daemon is draining — but what it does there is gated on the invariant below.

- **Every claim ends in a release or a restart — there is no third outcome**: a claim the daemon neither gives back nor restarts out of disables *all* config change for the life of the process image, because the slot is a plain in-memory bool. Three doors close a claim: `Apply` releases on a failed commit; the restart clears a committed one; and `abandonStagedApply` — called from `finishRunner` on every non-shutdown runner exit — covers the loop that died between `stageApply` and `runStagedApply` (a recovered panic keeps the daemon alive, so nothing else ever would). Abandoning also answers the call the dead loop owed, since nothing was written; `takePendingApply` is exactly-once, so the net is a no-op on every healthy path. The strand is only ever a *same-process* hazard: `stagedCalls` and the slot both die with the image, and a claim that never committed leaves no marker, so the next boot's sweep (PASS 0) finds the transcript's config call unowned and closes it.

- **A marker is armed only for a durably suspended call**: the marker is a promise to answer one exact `{session_id, tool_call_id}` after the restart, and only the durable transcript can back that promise. `runStagedApply` therefore re-reads it (`suspendIsDurable` over `storedExternalCalls` — the same name-keyed scan PASS 0 uses) and commits only when the call is there, unresolved, under its own tool name. Committing without it arms a marker no boot can consume: `DeliverPendingCallResult` fails on a call the transcript does not carry, the session is alive so `verdictUndeliverable` reads the failure as transient, and the marker survives every boot until the first unrelated startup failure rolls a config that has been live for days back to its backup — the exact hole ADR-0015 closed. A change that fails the check is dropped in-process instead: `ReleaseApply`, nothing written, and the ledger entry goes too, because a transcript with no such call has nobody waiting for an answer. When the transcript *read* fails the change is dropped the same way, but the call is answered through `enqueueSessionInput` — an unanswerable pending call bricks a session, and a wasted answer does not.

### Test patterns

Tests use `newSvc()` (internal constructor, no lifecycle side effects) with real SQLite databases (`migrate.OpenDB` on `t.TempDir()`), real `Store`/`sessionstore.Store`/`LinkStore`, and mock `session.Service`/`session.Factory`. Manager tests exercise concurrency scenarios (parallel sends, kill during run, clear, tool notifications) with `mockSession` that supports configurable delays and error injection. The shared daemon scenario harness models crash after inbox promotion and verifies the recovered response through the PubSub trace consumed by the production Telegram renderer. `linkstore_test.go` covers the ledger CRUD directly. Store tests verify the `work_dir` uniqueness behavior.


<a id="pkg-session"></a>

## `internal/session`

_Per-task ReAct loop, conversation history, compaction, loop detection, tool-execution orchestration, subagent delegation._


### Cross-cutting insight

The session package is both the composition root for per-task execution AND the ReAct loop implementation. It bridges the daemon layer (persistence, scheduling) and the tool layer (tool implementations) by owning the wiring between them, the conversation state (message history via `messageStore`), the LLM interaction protocol, compaction as the single context-pressure response, and loop-safety mechanisms (loop detection, empty-response guards). It also owns the persistence boundary — session records, conversation messages, clearing, and compaction all live in the session's `Store`, not the daemon's.

The `messageStore` struct encapsulates all message CRUD + persistence. The `svc.ms` field holds the `*messageStore`. Every write is **durable-first**: serialize → `InsertMessage` → stamp the id → only then append to the in-memory slice. A store failure is returned to the caller (never logged and swallowed), so the transcript can lag the DB but never lead it. Stored message content is immutable after insert: clearing and compaction are metadata events (`cleared_at`, `compacted_at`) plus appended rows, and the in-memory message list is a projection computed at load — a cleared tool result renders as a uniform placeholder built from its `tool_name`, never from the stored content.

A fresh-schedule **reset** (`ResetContextAndInject` → `ms.resetTo`) is one SQLite transaction: mark the previously active transcript `compacted_at`, insert the complete new opening turn, and clear stored compaction brief/todos. Any failure rolls every step back, so restart sees the old transcript and derived state intact — never a partial new header, two active transcripts, or cleared state paired with an old transcript. `ResetContextAndInjectOnce` additionally claims the producer delivery ID/fingerprint in that same transaction. Only a winning commit replaces the in-memory projection and clears its brief, todos and loop-detector window.

---

### 1. Purpose

The `session` package creates, configures, runs, and persists isolated agent sessions. Each session binds together an LLM client, a tool registry, MCP connections, memory services, a todo store, and a conversation history into a single unit of work that can be started fresh, resumed from SQLite, and shut down cleanly. A subagent is just another session of the same kind — a `sessions` row with `parent_id != 0` and an `agent_type` — driven by the same `runSession`/`runLoop` machinery; there is no separate lightweight session type or in-session fork engine.

Since the dissolution of the `agent` package, session also directly implements the ReAct loop — the iterative cycle of LLM call, tool execution, observation recording, and termination detection. It owns the conversation state (message history via `messageStore`), compaction as the single context-pressure response, and loop-safety mechanisms (loop detection, empty-response guards).

The daemon layer above speaks in session IDs and prompts. The tool layer below speaks in tool names and results. Session is the sole orchestrator.

### 2. Key types

| Name | Exported | Role | Owns |
|---|---|---|---|
| `Service` | yes (interface, 14 methods) | Public API for a session instance | `RunDaemon`, `PrepareUserMessage`, `SetModel`, `AgentTypes`, `RegisterGatedTool`, global `PendingExternalCalls`, exact/idempotent `ResolvePendingCall`, guarded `InjectToolNotification`/`ResetContextAndInject` plus producer-identified `...Once` variants returning an applied receipt, `AppendDeliveredCompletion`, `HasPendingWork`, `Close`. Pending identity is one `PendingToolCall{ID, Name}` value; the old independent `PendingSleepCallID`/`IsPending`/`InjectToolResult` surface is gone |
| `svc` | no (struct) | Session implementation of `Service` (same struct for top-level and child sessions); also implements the package-private `compactor` interface consumed only by its own `compact_context` tool | LLM client, `ms` (`*messageStore`), `loopDetector`, `compactionBrief`, `compactionFocus` (one-shot `/compact` focus), `baseline`/`modelEpoch` (the provider-measured compaction trigger, under `modelMu`), `pendingCompaction`, `suspended`, tool registry, `agentTypes` (`*registry.Set`), todo store, `stack` (`*builtin.Stack`), stamper, prompt builder (`*promptBuilder`), `loopOpts`, `maxIterations`, close-once guard, `id`/`rootID`/`agentType`, `cfg`, `newLLMWithModel`. **No `spawner`/`Spawner` field** -- the daemon injects `task`/`get_subagent_result`/`send_to_subagent` via `RegisterGatedTool` from the outside after the session is built |
| `messageStore` | no (struct) | Conversation message CRUD with optional persistence | `mu` (sync.Mutex), `messages []llmwire.Message`, `store sessionstore.RuntimeStore` (nil for in-memory-only), `sessID` |
| `loopRunner` | no (struct) | Per-invocation state for a single `runLoop` call | `agent *svc`, `opts loopOptions`, `cb iterationCallback`, `result *loopResult`, `log *zap.Logger`, `emptyCount int`, `lastResp *llmwire.Response` — all scoped to one run |
| `loopDetector` | no (struct) | Sliding-window diversity tracker for tool call patterns | `window`, `warnFingerprint`, `warnActive`, `blocked`, `consecutiveBlocks`, `forceTextOnly` |
| `heartbeatTicker` | no (struct) | Background goroutine that fires periodic heartbeat signals | `fn`, `mu` (sync.Mutex), `cancel` (context.CancelFunc) |
| `loopResult` | no (struct) | Final result of running the agent loop (FinalResponse, Iterations, Error, Suspended) | No behavior — pure data |
| `loopOptions` | no (struct) | Per-run runtime callbacks | `Notify`, `Heartbeat` |
| `InputBoundary` / `PendingInput` | yes | Session-owned consumption seam for durable normal input: peek, atomically accept/promote, reject, or handle a deterministic command | Implemented by daemon over `sessionstore.InboxStore` |
| `assistantState` | no (struct) | Snapshot of last assistant message state for resume handling | `HasPendingTools`, `PendingTools`, `HasText`, `Text` — analysis result |
| `iterationCallback` | no (func type) | Called after each iteration with response data | N/A |
| `sessionStatus` | no (struct) | Session statistics snapshot for `/status` command | Two honest numbers: current window occupancy (last-turn real `PromptTokens` / `contextWindow`) and lifetime session total (DB tree-sum, all-in) — plus model, iteration, subagent count. Pure data |
| `promptBuilder` | no (struct) | Encapsulates system prompt assembly (base + tools + skills + subagents + active subagents + memories + models) | `mu` (RWMutex), `basePrompt`, `toolsSection`, `skillsSection`, `subagentsSection`, `activeSubagentsSection`, `memoriesSection`, `modelsSection` |
| `Factory` | yes (interface) | Creates isolated session instances via `Create(ctx, CreateOptions)` | N/A (stateless beyond shared deps) |
| `factory` | no (struct) | Implementation of `Factory` | Reference to shared deps (config, memory, store, git, MCP pool, marketplace cache) |
| `CreateOptions` | yes (struct) | Factory creation options: identity/workdir/model, resume state, skills, active-subagent summaries, staged external calls, and `InputBoundary`. It carries no prompt | N/A |
| `params` | no (struct) | Per-session dependency bag passed to `newWithOptions` (unexported as part of the session-package unexport pass -- external callers go through `Factory`/`CreateOptions`) | N/A (plain data) |
| `options` | no (struct) | Per-session creation options (ID, resume data, agent type, reasoning level, LastActivityAt) | N/A (plain data) |
| `ActiveSubagentInfo` | yes (struct) | `ChildID`, `Blocking`, `State` -- the daemon-pushed set of a session's in-flight children, rendered into the "# Active subagents" prompt section | N/A (plain data) |
| `toolCallResultItem` | no (struct) | Result of a single tool call execution (index, toolCall, result, err) | N/A (transient) |
| `RunResult` | yes (struct) | Return value from `RunDaemon` (`FinalResponse`, actual loop `Suspended` discriminator) | N/A (plain data) |
| `timestamper` | no (struct) | Prefixes user messages with elapsed time and wall clock | `lastActivity` timestamp, optional `nowFunc` for testing |
| `toolRecord` | no (struct) | Loop detector record: `name`, `argsHash`, `resultHash` | N/A (transient) |
| `argKey` | no (struct) | Fingerprint key for Jaccard comparison: `name`, `argsHash` (excludes resultHash) | N/A (transient) |
| `loopAction` | no (int type) | Detector verdict: `actionNone`, `actionWarn`, `actionBlock`, `actionForceTextOnly` | N/A |

`Notification` lives in `internal/sessionevent`; `RunDaemon` receives only its notification callback. Persistence types live in `internal/sessionstore`; a live session accepts only `RuntimeStore` plus the narrow `InputBoundary`, so it cannot create/kill sessions or know controller transport state.

### 3. File map

- **session.go** — `Service` interface, `svc` struct, `newWithOptions` constructor, `setupRegistry`/`refreshRegistrySections` (every prompt section derived from the live registry — tools inventory, skills gated on `skill`, subagents gated on `task`), registry/gating/model/resource methods, guarded `InjectToolNotification`/`ResetContextAndInject` and their durable-identity `...Once` variants, separate `BuildBlockingSubagentCompletion`/`BuildBackgroundSubagentCompletion`, `AppendDeliveredCompletion`, the compaction-request trio (`RequestCompaction`/`compactionRequested`/`consumePendingCompaction`), `setCompactionFocus`, and `ActiveSubagentInfo`/`ActiveSubagentsProvider`
- **external_call.go** — `PendingToolCall`/`CallResolution`, whole-transcript pending-state scan, exact/idempotent resolution with identity checks, narrow producer-owned loop blocking vs wide repair protection
- **run.go** — `Run`, `RunDaemon(notify)`, the activation-start `refreshRegistrySections()` call, fresh-session initialization, and `/status` rendering (occupancy is the compaction trigger's own projection, tilde-marked when no provider measurement backs it)
- **input_boundary.go** — durable normal-input contract (`InputBoundary`, `PendingInput`); no wake or steering primitive
- **skill.go** — `PrepareUserMessage`, exact leading `/skill <name> [args]` parser, user-visibility validation, canonical skill-envelope expansion
- **loop.go** — `runLoop`, previous-result settlement, LLM call (which records the provider-measured context baseline), termination and finalization
- **loop_boundary.go** — `drainBoundary` (causal durable-input consumption), `handleBoundaryCommand` (`/status`, `/compact`), `handleCompactCommand` (raises the compaction flag; interrupts sleep, defers behind any other pending call by leaving the input in the inbox), `nothingToAnswer` (the shared guard: a boundary command or rejected input ends the activation only when the transcript owes the model nothing), `boundaryOutcome`, `interruptSleeps`, `onlySleepCalls`
- **loop_context.go** — `applyContextEvents` (the single sanctioned compaction point: refuses while a tool call is pending, forces on an explicit flag, otherwise decides on the projection) and `recordAutoCompaction` (silences the automatic path after `compactionAttemptCap` fruitless attempts)
- **message_store.go** — `messageStore` struct, message CRUD (`addUserMessage`, `addAssistantMessage`, `addToolResult` -- each returns `error` only; the DB id is stamped onto the message, not handed back), `appendMessageLocked()` (the durable-first append), `getMessages()`, `setMessages()`, `appendPersisted()`, `reloadMessages()`, `replaceCompactedMessagesLocked()`, `persistCompactionBrief()`
- **message_persist.go** — the write paths that talk to the store in their own shapes: `storedMessage()` (llmwire → `sessionstore.StoredMessage`), `addToolNotificationPairOnce()` (synthetic assistant stub + tool_result committed together with the durable delivery identity), `resetToOnce()` (claim the delivery identity, insert the new opening turn, then mark every previously active row `compacted_at` -- a full-transcript hide, distinct from compaction's partial clear/replace)
- **toolexec.go** — `toolCallResultItem`, parallel tool execution (`executeToolCalls`, `executeToolCallsInternal`), `batchConflict` (preflight rejection, before any concurrent side effect, of `sleep` alongside `task`/`send_to_subagent` and of `compact_context` alongside any suspending call — the flag it raises would die with the svc a suspend rebuilds), `recordToolResults` (appends each result in call order, stops on the first failed write), `prependLoopWarning` (fronts the last result with the detector's warning), loop detector integration, `executeToolCall`, `formatToolResult`, `countUniqueOutcomes`
- **compaction.go** — `compact()` / `compactLocked()` ([ADR-0012](docs/adr/0012-compaction-is-all-or-nothing.md), [ADR-0013](docs/adr/0013-immutable-history-single-compaction-point.md)): a pending-call guard (defence in depth behind `applyContextEvents`'s own check), `applyClear` at `keepRecent` so the tool bodies about to be replaced go in as placeholders, `headerFitsLocked` (the untouchable header plus the system prompt must stay under the trigger, else `errCompactionHeaderTooLarge` — compaction cannot converge otherwise), then ONE LLM call over the whole remaining conversation. `summarizeStartAfter` skips everything a previous compaction wrote (summary, ack, primer, reattachments), so a repeat `/compact` answers "nothing to compact" without a call. `rebuildMessages` keeps NO verbatim tail: header → summary turn → ack → primer → reattachments, and the compacted range spans everything after the header. Compaction either produces a real summary or fails — over budget, cancelled, transport or provider failure all abort and leave the conversation in place, with no placeholder brief and no partial summary. A failed summarization keeps the trim it already applied, and the in-memory brief advances only after the durable swap lands
- **compaction_summarize.go** — `compactInitialLocked`/`compactMergeLocked` (the incremental path omits only the previous summary/ack/primer rows, whose brief is passed in directly), `compactionInputBudget` (context window minus `compactionOutputReserve` — NOT the `compactionFraction` trigger: the payload IS the conversation, which is above that fraction whenever the automatic path fires), `compactWithRetry`/`validateSummary` (the section contract binds the final brief, the only output there is), `compactOnce` (budget check, then the call carrying `llmwire.WithMaxTokens(compactionOutputReserve)`)
- **compaction_summary.go** — `summaryTurn` and its render: the model's brief plus the two things a paraphrase cannot carry — `buildVerbatimTail` (last 3 substantive user/assistant turns, 600 chars each, synthetic rows and tool results excluded) and `activeBackgroundSection` (children still running, read live through `ActiveSubagentsProvider`). Neither enters `compactionBrief`, so the next merge is not fed its own quotes
- **compaction_reattach.go** — `selectSkillReattachments` (latest invocation per skill, newest first), `skillReattachBudget` (10% of the context window; a fixed 25k was 92% of the trigger on a 32k window), per-skill cap `min(skillReattachMaxTokens, combined)`, and skip-whole-candidate-on-overflow rather than trimming to the remainder
- **loopdetect.go** — `loopDetector` state machine, `loopAction` constants, `toolRecord`, `argKey`, diversity scoring (`uniqueArgsFraction`, `uniqueResultsFraction`), Jaccard similarity (`jaccardSimilarityArgs`, `jaccardSimilarity`), FNV fingerprinting (`fingerprintArgs`, `fingerprintResult`), warn/block/forceTextOnly escalation, `resetWindow`, `clearForceTextOnly`
- **rounds.go** — `findMaskBoundary` (walks messages backwards counting complete rounds; the boundary `applyClear` protects)
- **clear_context.go** — `clearedPlaceholder` (uniform placeholder from `tool_name`, shared by load and event-apply paths), `applyClear` (compaction's first phase and its only caller: select eligible `role='tool'` rows per the exclusion rules, `MarkCleared` in DB, substitute placeholders in memory), `clearEligible`
- **compact_context.go** — `compactor` interface, `compactContextTool` (`compact_context`, defers via `RequestCompaction`), `newCompactContextTool`
- **heartbeat.go** — `heartbeatTicker` — background goroutine for periodic activity signals
- **repair.go** — `repairTranscript` (fixes orphaned/missing/misordered/duplicate tool results), `repairTranscriptExcluding` (skips pending external call IDs), `emitToolResults`, `hasIncompleteToolCalls` — all package-private
- **truncate.go** — `truncateHeadTail` — head+tail truncation with newline snapping (`snapToNewline`) and error-tail detection (`hasImportantTail`)
- **serialize.go** — `estimateText`/`estimateTokens`/`estimateSchemas` (the `len/4` rule), `contextBaseline` (the provider's last cache-inclusive `PromptTokens` plus the transcript position it covered), `projectContextSize` (baseline + `len/4` of the tail appended since; with no baseline, the whole transcript plus system prompt and tool schemas), `recordContextBaseline`/`resetContextBaseline`/`modelGeneration`, and `shouldCompact(window)` (`projection > compactionCutoff(window)`; `compactionFraction = llmwire.ContextInputFraction` = 0.85, shared with the `llm` client's `max_tokens` clamp so the input and output reserves stay complementary). The baseline is written only in `callLLM` — the compaction call goes through `s.chat` directly, so its own usage never becomes the next trigger — and is dropped whenever it stops describing the transcript: after clearing, after a rebuild, on a model switch, and on a context reset
- **factory.go** — `Factory` interface, `factory` struct, `CreateOptions` (incl. `ActiveSubagents`), `Create`, `build` (calls `builtin.BuildStack` to get a `*builtin.Stack`, closes it on any construction error so a failed build never leaks LSP/MCP), `sessionConfig`, `FactoryOption`/`WithLLMClientFactory`/`NewFactoryWithOptions` (test seam for injecting a scripted LLM client). Holds the `mcpstore.Store` and the `config.Secrets` map that `resolveMCPServers` needs
- **mcpservers.go** — `resolveMCPServers(ctx, store, secrets, projectID)`: the project's registry rows → `map[string]mcp.ServerConfig`, expanding `${VAR}` env references via `config.Secrets.Expand`. A server whose reference cannot be resolved is skipped with a warn naming the variable (never a value); the rest of the set still starts. This is the single composition point a future repo-local MCP source merges into
- **setup.go** — `loadProjectContext` (marketplace/skills/subagents loading), `subagentConfigs` (converts `loader.Service.ListSubagents()` into `[]registry.AgentTypeConfig`, dropping a frontmatter `model:` the configured catalog cannot resolve with one `subagent_model_unknown` warning — [ADR-0014](docs/adr/0014-subagent-definitions-degrade-never-disable.md)), `modelConfigured`, `registerSessionTools` (curated-memory + `compact_context` tools only -- `task`/`get_subagent_result`/`send_to_subagent`/`schedule`/`sleep` are registered by the daemon onto the live registry after the session is built, since they need daemon state this package doesn't have; unlike those, this in-package registration is deliberately outside the agent-type gate — compaction and memory are session capabilities, not control plane, so every subagent has them regardless of its `Tools` list), `persistState`
- **model.go** — `validateModelSwitch`, `handleSetModel` (hot-swap LLM client mid-session), exported `ResolveReasoningLevel` (the single place an effort level is settled against a model's catalog entry; every daemon-side writer of `reasoning_level` routes through it)
- **prompt.go** — `promptBuilder` struct (`systemPrompt`, `setToolsSection`, `setSkillsSection`, `setSubagentsSection`, `refreshMemories`, `setModelsSection`, `setActiveSubagentsSection`), `buildToolsSection` (dynamic tool inventory + conditional guidance for memory, batch, scheduling, web search), `buildSkillsSection` (sorted model-visible inventory with bounded descriptions), `buildMemoriesSection` (each entry rendered as `- [id] text` — the id is the only handle `memory_delete` accepts), `buildModelsSection`, `buildSubagentsSection`, `buildActiveSubagentsSection`
- **timestamp.go** — `timestamper` struct, `stamp`/`touch`/`now`/`formatElapsed`, `localTimezone`

### 4. State ownership

| State | Location | Source of truth | Synchronization | Failure behavior |
|---|---|---|---|---|
| Session records (iteration, status, model, attributes) | SQLite (`sessions` table) | Store | `*sql.DB` (goroutine-safe) + SQLite locking | Persist error in `persistState` aborts the agent loop |
| Conversation messages | `svc.ms.messages` + SQLite (`messages` table) | SQLite; `messageStore` writes the row first and appends to its in-memory slice only after the insert returns an id | `messageStore.mu` (sync.Mutex) for all reads/writes | Durable-first: an insert error is returned to the caller, nothing is appended in memory, and the run fails. There is no DBID=0 in-memory-only state when a store is present. `reloadMessages` re-syncs from DB. |
| `compactionBrief string` | `svc.compactionBrief` | In-memory, persisted via `persistCompactionBrief` | Read/written only from the loop goroutine (single-goroutine per session, top-level or child alike). | Persist failure on the compaction path: logged, brief stays in memory; on restart, restored from DB. On the `ResetContextAndInjectOnce` path the failure is returned and the reset aborts. |
| `pendingCompaction *int` | `svc.pendingCompaction` | In-memory flag, consumed once per loop iteration | `messageStore.mu` (set by `RequestCompaction`, peeked by `compactionRequested`, consumed by `consumePendingCompaction`) | No persistence. A `/compact` behind a pending call is therefore never turned into a flag — it stays in the durable inbox until the call settles. |
| `suspended bool` | `svc.suspended` | In-memory flag, set by `recordToolResults` on `ErrSuspend` | No mutex — written only in tool execution goroutine join, read in `handlePreviousResult` (same goroutine) | Not persisted. Loop exits when set; session handles checkpoint. |
| `loopDetector` | `svc.loopDetector` | In-memory only | Single loop goroutine | Resets when a durable message is successfully promoted; lost on crash |
| `baseline *contextBaseline` + `modelEpoch` (compaction trigger) | `svc.baseline` | In-memory only | `modelMu` — written in `callLLM` (loop goroutine) and cleared by `handleSetModel` (daemon goroutine); `callLLM` samples `modelEpoch` before the request and its write is dropped if a switch landed meanwhile. | Not persisted. Absent after resume, so the first post-resume check runs on a pure `len/4` estimate — self-heals on the next real response. |
| `loopRunner.*` | stack-local to `runLoop` | Exists only during one `Run`/`RunDaemon` call | Single goroutine — no sync needed | Dies with the call. |
| `loopOpts` | `svc.loopOpts` | Set by `RunDaemon` or `Run` before loop starts | No mutex — written once before loop, read during loop from same goroutine | N/A |
| System prompt (base + tools + skills + subagents + memories + models) | `svc.prompt` (`*promptBuilder`) | promptBuilder struct fields | `promptBuilder.mu` (RWMutex) | N/A (read-only after init except for model switch, tool registration, and memory refresh) |
| LLM client + model triplet | `svc.llmClient`/`model`/`reasoningLevel` | Session struct fields | `chat` holds `modelMu.RLock` for the complete provider call; `handleSetModel` takes the write lock before swapping, then closes the unreachable old client outside the lock; `closeLLM` excludes both | Old client close error is logged, not propagated |
| Todo items | `svc.todoStore` (`todo.Service`) | In-memory; serialized to SQLite via `persistState` | Single-goroutine (agent loop) | Marshal/persist error aborts the loop |
| Subagent concurrency | DAEMON admission controller (`daemon/admission.go`) | Daemon-level slot counters | Kind-aware caps (total/child/per-parent/depth) + FIFO queue for background overflow — outside this package. See the [`daemon`](#pkg-daemon) section. | Context cancellation / cap rejection surfaces as a `Spawn` error |
| Subagent completion owed | SQLite (`subagent_links` table; delivery CAS in `sessionstore/delivery_store.go`) | `sessionstore.OrchestrationStore.DeliverCompletionAtomic` (the two `delivered_*` columns); rest of the ledger by `daemon.LinkStore` | Single transaction per state change; delivery CAS + message insert in one tx (`DeliverCompletionAtomic`) | Durable: survives restart/compaction; redelivery dedup via `delivered_at` alone |
| Timestamper | `svc.stamper` | `lastActivity` field | Single loop goroutine; `stampAt` preserves inbox receipt time | N/A |
| Close guard | `svc.closeOnce` | `sync.Once` | Built-in | Idempotent by design |
| Message compaction | SQLite (`messages.compacted_at`, `messages.position`) | Store via `ReplaceCompactedMessages` | One transaction for soft-delete, retained-row reposition, and synthetic-row insert | Transaction rollback leaves the prior transcript intact |
| Kill flag | SQLite (`sessions.killed_at`) | Store via `MarkSessionKilled` | Transaction: kill session + delete schedules atomically | Transaction rollback on failure |

### 5. Concurrency model

**Goroutines:**

1. **Main loop goroutine** — runs causal boundary consumption, LLM calls, context events and tool-result collection. It is the single writer for `messageStore`, `loopDetector`, and `suspended`.

2. **Heartbeat goroutine** — spawned by `heartbeatTicker.start()`, fires `opts.Heartbeat` every 1 second. Panic-recovered. Cancelled by `heartbeatTicker.stop()` (deferred in `runLoop`). No shared state with the main loop except the parent context.

3. **Tool execution goroutines** — `executeToolCallsInternal` spawns one goroutine per tool call via `sync.WaitGroup`. Results written to pre-allocated `results[idx]` (no contention — each goroutine writes its own index). Panic-recovered per goroutine. All join before returning to the main loop goroutine.


**Mutexes:**

- `messageStore.mu` (sync.Mutex): protects `messages` slice. Used by all message CRUD methods, `reloadMessages`, `compact`, `applyClear`.
- `promptBuilder.mu` (RWMutex): protects `basePrompt`, `toolsSection`, `skillsSection`, `subagentsSection`, `activeSubagentsSection`, `memoriesSection`, `modelsSection`. Read-locked by `systemPrompt()` (called from loop). Write-locked by `setToolsSection`, `setSkillsSection`, `setSubagentsSection`, `setModelsSection`, `setActiveSubagentsSection`, and `refreshMemories`.
- `heartbeatTicker.mu` (sync.Mutex): protects `cancel` field (start/stop serialization).
- `svc.closeOnce` (sync.Once): guards `Close` idempotency.

**Cross-goroutine communication:**

- Durable normal input crosses the package boundary synchronously through `InputBoundary`; there is no message channel or concurrent transcript writer.
- Tool execution results: communicated via indexed slice write (goroutine i writes `results[i]`), synchronized by `WaitGroup.Wait`.

**Inherited tensions:**

- **Compaction holding mutex during LLM call**: `compact()` holds `messageStore.mu` for the entire function including LLM summarization calls (`compactInitialLocked`, `compactMergeLocked`). This is safe *only* because of the single-goroutine invariant — the loop goroutine is the sole caller of `ms.mu`. The invariant is load-bearing: introduce any second goroutine that takes `ms.mu` and it can be starved for the full summarization call (bounded, but up to ~6×`defaultModelTimeout`), turning today's benign latency note into a real stall. A future improvement would snapshot messages under lock, release, run LLM, re-acquire to rebuild.
- **`appendMessageLocked` does DB IO under mutex**: All message CRUD methods hold `messageStore.mu` during `store.InsertMessage`, and since the write is durable-first the IO now precedes the slice mutation rather than following it — the lock is held across the same call either way. Same single-writer mitigation applies.
- **`llmClient`/`model`/`reasoningLevel` guarded by `modelMu`**: `chat` keeps `RLock` for the entire external provider call. `handleSetModel` must acquire the write lock, so every old-client Chat has finished before the swap; after unlock, new readers can reach only the new client and the old one is safe to close. `closeLLM` also takes the write lock. `currentLLM` is only for short, non-resource reads such as `ContextWindow`.

### 6. Data flow

#### Normal iteration (LLM calls tool, tool returns result)

```
runLoop
  -> if an external call is already staged: let boundary inspect it (sleep may be interrupted)
  -> handlePreviousResult (settle prior tools/final text first)
  -> drainBoundary (FIFO promote only when no non-sleep external call blocks it)
  -> applyContextEvents (the single compaction point; see below)
  -> reloadMessages (re-sync from DB)
  -> callLLM (svc.llmClient.Chat -> llmwire.Response)
  -> recordIteration (increment counter, fire callback, addAssistantMessage)
  -> [next iteration] handlePreviousResult
    -> lastAssistantState (find pending tool calls)
    -> executeToolCalls
      -> loopDetector.check (pre-check: block?)
      -> executeToolCallsInternal (parallel goroutines -> tool.Execute)
      -> loopDetector.record + check (post-check: warn?)
      -> addToolResult (per result, with warning prepended if ActionWarn)
  -> [loop continues]
```

Tool results are never rewritten in place after execution — the transcript is append-only. Nothing edits history between compactions ([ADR-0013](docs/adr/0013-immutable-history-single-compaction-point.md)); older content is dropped only by the compaction event below.

#### Compaction (applyContextEvents)

```
applyContextEvents
  -> HasPendingExternalCall() || HasPendingWork()? -> return  <- the safety decision
  -> consumePendingCompaction (/compact or compact_context?) -> force (bypass predicate)
  -> if NOT explicit:
       -> automatic path silenced by the attempt cap? -> return
       -> shouldCompact(window)? no -> return
  -> svc.compact(keepRecent)
    -> pending-call guard (defence in depth)
    -> applyClear(keepRecent)               <- phase 1: drop old tool-result bodies
    -> activeBackgroundSection (live ledger read, before taking ms.mu)
    -> headerFitsLocked? no -> errCompactionHeaderTooLarge
    -> summarizeStartAfter (skip the previous compaction's own output) -> nothing new? -> (false, nil)
    -> selectSkillReattachments (everything after the header is eligible)
    -> compactInitialLocked OR compactMergeLocked (ONE LLM call, capped at compactionOutputReserve)
    -> rebuild message slice: [header, summary turn, ack, primer, skill reattachments]  <- no tail
    -> store.ReplaceCompactedMessages (soft-delete + stable-ID order + synthetic inserts, one tx)
    -> store.PersistCompactionBrief
  -> automatic path: projection still over the threshold? -> count it; cap reached -> go quiet
```

The summary turn is one `RoleUser` row carrying the model's brief, a programmatic verbatim excerpt (last 3 substantive turns, 600 chars each) and the active-subagent section; only the brief is persisted as `compactionBrief` and fed to the next merge. Each reattached skill is a `RoleUser` canonical envelope capped at 5,000 estimated tokens, with the combined set capped at 10% of the context window. Selection prefers the most recent invocations, then restores chronological order.

#### Termination (assistant produces text-only response)

```
handlePreviousResult
  -> lastAssistantState -> HasText=true, no pending tools
  -> set result.FinalResponse
  -> opts.Notify (deliver final text)
  -> return done=true -> runLoop returns
```

#### New session creation (factory path)
```
daemon.Manager
  -> factory.Create(ctx, CreateOptions)
    -> factory.sessionConfig()           // clone config with workdir/model
    -> llm.NewClient(cfg)                // create LLM client
    -> newMessageStore + reloadMessages  // try loading messages from DB (resume detection)
    -> factory.build()
      -> builtin.BuildStack(ctx, StackConfig{WorkDir, Pool, Unified, Loader, Todo})
           -> tool.NewRegistry(), lsp.NewManager(provider), registerCoreTools() (read, write, edit,
              apply_patch, ls, glob, grep, bash, webfetch, skill, todoread, todowrite,
              batch, lsp -- fixed order, feeds the prompt-cache key)
           -> mcp.AcquireForWorkDir() (pool-or-direct) -> mcpSvc.RegisterTools()
           <- *builtin.Stack{Registry, lspMgr, mcpSvc}  (Close() on any later failure)
      -> newWithOptions()
        -> loadProjectContext()           // marketplaces, AGENTS.md, skills, subagents
        -> registry.NewSet(projectSubagents) -> set.Get(agentType)   // setup before resolution
        -> newPromptBuilder(base, "", memories, models)
        -> newMessageStore(store, id)    // create message store
        -> filterRegistryForAgent(set, reg, agentConfig)   // filter tools by agent type
        -> registerSessionTools()        // curated-memory + compact_context tools only
        -> refreshRegistrySections()     // tools/skills/subagents inventories from the live registry
        -> buildActiveSubagentsSection(opts.ActiveSubagents)
        -> persistState(0, "active")     // write initial state to DB (fresh only)
      <- Service (svc.stack = the *builtin.Stack)
    <- Service
  <- Service

(daemon.runner then calls registerScheduleTools/registerSubagentTools on the live
 registry -- see the daemon section -- adding task/schedule/sleep/get_subagent_result/
 send_to_subagent, each gated by the session's own AgentTypes().FilterTools allowlist;
 svc.run() re-runs refreshRegistrySections() so the first request of the activation
 describes that final set)
```

#### Subagent spawn
A subagent is a first-class session created by the daemon, not an in-session fork.
The session doesn't spawn anything itself, and holds no `spawner`/`Spawner` field at
all: the `task` tool (and `get_subagent_result`/`send_to_subagent`) are Go types
defined **in the daemon package** (`daemon/task.go`, `subagent_result.go`,
`subagent_send.go`), registered onto the session via `RegisterGatedTool` by
`daemon.runner` after the session is built (`registerSubagentTools`; the session
applies its own `AgentTypes().FilterTools` gate). The session's only role is to run
whatever tool ends up in its registry -- it never imports the daemon's spawn logic.
Two modes, from the daemon's side:

```
daemon's taskTool (inside the parent's agent loop, holding the daemon's own spawner)

  BLOCKING (task tool returns ErrSuspend)
  -> spawner.LinkPending(parentID, taskCallID)   // already linked? -> just suspend
  -> spawner.Spawn(spawnRequest{Blocking:true, TaskCallID, ParentID, RootID, AgentType, Depth, ...})
  -> return ErrSuspend                             // parent loop exits, RELEASING its run-slot
                                                    // (kills priority-inversion deadlock)
     ... later, on child completion the daemon resumes the parent and fills the
         original task tool_use's tool_result atomically (sessionstore.DeliverCompletionAtomic).

  BACKGROUND (task tool returns immediately)
  -> spawner.Spawn(spawnRequest{Blocking:false, ...})  // returns child id now
  -> return child id to the model
     ... later, on child completion the daemon delivers a synthetic subagent_event
         tool_call+tool_result pair (built by session.BuildSubagentCompletion, committed by
         sessionstore.DeliverCompletionAtomic).
```

The durable `subagent_links` row (`daemon.LinkStore`) records that a completion is
owed and survives restart/compaction; `sessionstore.OrchestrationStore.DeliverCompletionAtomic` is the
one link-adjacent write this package still owns (see [Cross-package contracts](#cross-package-contracts)).
Admission control, completion delivery, and cascade-kill all live in the daemon
package — see the [`daemon`](#pkg-daemon) section.

### 7. Lifecycle

#### Session state machine
```
                    +---> [error]
                    |
[init] --persist--> [active] --loop--> [active] --exit--> [completed]
                       |                  |
                       |                  +--external obligation--> [suspended]
                       |                  +--/stop--> [stopping] --> [stopped]
                       |
                       +--resume--> [active] (iterationOffset > 0)
```

States are string values written to SQLite via `persistState`:
- **init**: Transient; exists only during `newWithOptions` before first `persistState(0, "active")`
- **active**: Written after creation and after each agent iteration
- **completed**: Written when the agent loop exits normally without suspension
- **suspended**: Written when the agent exits with a pending external call (sleep, foreground subagent, config/secret result)
- **stopping/stopped**: Daemon-owned durable two-phase parking; explicit input resumes a stopped root or child according to policy
- **error**: Written when the agent loop returns an error

#### svc lifecycle
1. Constructed via `newWithOptions` (or `factory.Create` -> `build` -> `newWithOptions`)
2. `RunDaemon` is called with a notification callback; durable input is obtained through the factory-supplied boundary.
3. A fresh session initializes its header; subsequent messages are promoted at loop boundaries.
4. ReAct loop runs iterations (LLM call -> tool execution -> persist), managed by `loopRunner`
5. Loop exits -> final status persisted ("completed", "suspended", or "error")
6. `Close` called (releases LLM client, closes `svc.stack` -- `Stack.Close()` stops the LSP manager and the MCP service/pool-view, once via `sync.Once`)

#### loopDetector escalation ladder
```
ActionNone -> ActionWarn -> ActionBlock (x3) -> ActionForceTextOnly
                ^                                       |
                +-- clearForceTextOnly (on text response)+

resetWindow() -> resets everything (on accepted normal input)
healthy diversity -> resets warn/block state
```

#### Subagent lifecycle
1. Daemon's `task` tool (registered onto the parent via `RegisterGatedTool` from outside) calls the daemon's own `spawner.Spawn(spawnRequest{...})` -> daemon admits (or queues) and creates a `sessions` row with `parent_id != 0` plus a `subagent_links` row (`daemon.LinkStore`)
2. Daemon drives the child via the same `runSession`/`runLoop` as any session — its own `svc`, `messageStore`, and iteration cap (`agentConfig.MaxIterations`)
3. Loop exits -> daemon conditionally finalizes the activation through `sessionstore.TryFinalizeSubagentActivation`; a pending follow-up keeps the activation running, while `/stop` wins through persisted status. A terminal outcome is delivered exactly once through `DeliverCompletionAtomic`
4. `child.Close()` releases the child's LLM client and closes the child's own `*builtin.Stack`. The child does NOT close a *shared* Stack -- each session (parent and child alike) gets its own `builtin.BuildStack` result; only the owning/root session's Close matters for anything it shares (e.g. a pooled MCP client's refcount)

### 8. Tensions

1. **Store owns both session and message persistence but has no in-memory cache**: Every `persistState` call and every `LoadActiveMessages` call hits SQLite directly. For high-iteration sessions (hundreds of iterations), the per-iteration persist creates sustained write pressure. There is no batching or write-behind — each iteration is a synchronous DB write.

2. **`loopDetector` accessed without `messageStore.mu`**: Safety relies on boundary consumption and tool execution joining back into the single loop goroutine before detector mutation.

3. **`suspended` flag set without mutex**: `suspended = true` in `recordToolResults` is a direct field write. It's read in `handlePreviousResult`. Both happen in the same `runLoop` goroutine, but the flag has no synchronization.

4. **Persistence IO under messageStore.mu**: `applyClear` calls `store.MarkCleared` (metadata only, never content) while holding `messageStore.mu`, coupling message access latency to DB write latency for the duration of a clear event.

### 9. Ordering constraints

1. **`handlePreviousResult` before `callLLM`**: Tool calls from the previous iteration must be executed and their results added to messages before the next LLM call. Reversing this would send the LLM a conversation with pending tool calls and no results.

2. **Previous result before ordinary boundary promotion**: a queued message must not hide an assistant tool call/final answer. Only a previously staged external call lets the boundary inspect input early, and only sleep is interruptible.

3. **`reloadMessages` after `applyContextEvents`**: Compaction restructures messages in-memory and persists the changes. ReloadMessages syncs from DB to pick up any concurrent modifications. If `reloadMessages` ran first, it would overwrite the compaction's result.

4. **`applyClear` substitutes placeholders in memory before the summarization prompt is built**: the prompt is assembled from the in-memory projection, so deferring the substitution to `reloadMessages` would send full tool bodies to the summarizer and blow the budget it was meant to fit.

5. **A `/compact` behind a pending call stays in the durable inbox**: it is never turned into an in-memory flag, because the svc that would hold it is rebuilt on resume. `drainBoundary` stops draining at that point rather than re-peeking the same input. The deferral notice is deduplicated per **deferral episode** (the lifetime of the pending call that caused it), not per run: the verdict travels `RunResult.CompactionDeferAnnounced` → daemon (`deferAnnouncements`, in-memory) → `CreateOptions.CompactionDeferAnnounced` on the next wake, and `runLoop` clears it once no external call is pending. A daemon restart mid-episode re-announces once — deliberate, the ledger is not durable.

6. **`loadProjectContext` before `registry.NewSet`/agent-type resolution**: It produces the `[]registry.AgentTypeConfig` that `registry.NewSet(projectSubagents)` overlays onto the built-ins; `set.Get(agentType)` must find a config before `filterRegistryForAgent` runs.

7. **`builtin.BuildStack` (core tools + MCP) before `registerSessionTools`**: Core tools populate the registry that `filterRegistryForAgent` copies. Session-dependent tools (memory, `compact_context`) are registered after construction; the daemon's own `registerScheduleTools`/`registerSubagentTools` run later still, once the session exists and admits them through `RegisterGatedTool`.

8. **`refreshRegistrySections` at activation start, after every registration**: the daemon registers `task`/`schedule`/`sleep`/config/MCP tools between `factory.Create` and the run, so the inventory computed at construction describes a set the session never runs with. `svc.run` recomputes it once, before the first request, and never again inside the loop — the prompt prefix must stay byte-stable across an activation's iterations for the provider's cache.

9. **`persistState` before loop exit**: The progress callback must persist state before returning. If persistence fails, the callback returns an error that aborts the loop.

10. **`MarkSessionKilled` is transactional (kill + delete schedules)**: Prevents orphaned schedules from firing for dead sessions.

### 10. Package anti-patterns

1. **Don't run the loop concurrently on the same `svc`**: The `loopDetector`, `suspended` flag, and `loopRunner.lastResp` are not synchronized for concurrent access. Two concurrent runs would corrupt the detector's window and race on message appends.

2. **Don't cache a `messages()` snapshot across a context event**: The returned slice is a copy, but `applyClear`/`compact` mutate `Content` in place on the underlying `messageStore.messages` during a clear/compaction event. Between events the content is byte-stable (append-only); a snapshot taken before an event will diverge after it.

3. **Don't call `Chat`/`Close` through a `currentLLM()` snapshot**: resource-using operations must go through `chat`/`closeLLM`, which hold the model read/write lease for the operation. Returning to snapshot-based calls would let a model switch close an in-flight backend.

4. **Don't pass `keepRecent < 2` to compaction**: `compact()` protects a dynamic one- or two-message header, but still needs enough complete recent rounds for `findMaskBoundary` and transcript repair to keep a valid tail.

5. **Don't add normal messages directly through `messageStore`**: built-in managers enqueue through `session_inbox`; the session alone promotes them in FIFO order.

6. **Don't call `Run`/`RunDaemon` more than once per session instance in production**: A daemon runner constructs a new live session service for each admitted run. Both public methods wrap the same private `run` implementation and are not alternatives to call sequentially on one instance; package tests may re-enter it to assert that per-run discriminators such as `suspended` are reset.

7. **Don't register tools into `svc.registry` after construction**: Tools must be added through `RegisterGatedTool` (which targets the session's live registry and applies the agent-type gate), not shoved into the root registry, to be visible to the running loop.

8. **Don't share a `todo.Service` between parent and subagent**: each session — child included — gets a fresh `todo.New()` at construction. Sharing would cause unsynchronized concurrent access.

9. **Each session's `Close` only tears down its own `*builtin.Stack`**: every session -- parent or child -- builds its own `Stack` via `builtin.BuildStack` (its own LSP manager, its own MCP acquisition). In pooled MCP mode this only decrements a refcount, so a child closing does not kill the parent's or a sibling's connections; in direct mode it closes a subprocess only that session started. There is no cross-session sharing to worry about breaking.

### 11. Contracts

1. **Message header invariant**: Compaction protects two messages when `messages[0]` is an AGENTS.md context message (or the test-only `RoleSystem` header), otherwise it protects only the initial task. The system prompt is NOT a production message — it is passed separately to `llmClient.Chat()`. Header detection must remain aligned with fresh-session message construction.

2. **Tool call/result pairing**: Every assistant `ToolCall.ID` must eventually have a matching tool result message with the same `ToolCallID`, or `lastAssistantState` will report pending tools and the loop will re-execute them. The only exception is `ErrSuspend`, where the result is skipped and later resolved by exact `{ID, Name}` identity through `ResolvePendingCall` or the blocking-completion transaction.

3. **Compaction brief accumulation**: `compactionBrief` is append-only across the session's lifetime. `compact()` chooses initial vs. merge path based on `compactionBrief == ""`. Clearing it externally causes the next compaction to regenerate from scratch, losing incremental context. The one sanctioned exception is `ResetContextAndInjectOnce` (fresh schedules), which deliberately clears both the in-memory field and the stored brief along with the whole transcript — starting the next run's compaction from scratch is the intent, not a bug.

4. **Loop detector reset on accepted normal input**: boundary drain calls `PrepareUserMessage`; only successful promotion resets the detector. Invalid skill commands are rejected without a reset.

5. **`keepRecent` bounds what the summarizer reads, not what survives**: `applyClear` uses `findMaskBoundary` (in `rounds.go`) to leave the last `keepRecent` rounds' tool output intact for the summarization prompt. Nothing is preserved verbatim past the rebuild, so `compactionKeepRecent` (6 rounds) and the `compact_context` parameter (default 6, min 4, max 20) are a cost/fidelity knob on the prompt, not a retention policy.

6. **Session ID is immutable after construction**: `svc.id` and `svc.rootID` are set in `newWithOptions` and never modified. All persistence operations use `s.id`. A child session gets its own `id` but inherits the parent's `rootID`.

7. **Every session owns its own `*builtin.Stack`**: unlike the pre-July design (a single MCP manager shared and stopped only by the root session), each session -- including subagents -- builds an independent `Stack` (own LSP manager, own MCP acquisition via `mcp.AcquireForWorkDir`). `Close()` always tears down that session's own Stack; in pooled MCP mode this is a refcount decrement, so it's safe regardless of which session (root or child) calls it.

8. **System prompt is always read through `promptBuilder.systemPrompt()`**: The method acquires `mu.RLock` and concatenates sections. It is passed as a method value to the loop. Reading fields directly would race with `setModelsSection` and `refreshMemories`.

9. **Cleared results render from structure, not content**: a cleared tool result's placeholder is built from its `tool_name` via the shared `clearedPlaceholder`, applied identically at load (`reloadMessages`) and event-apply (`applyClear`). The original content is never kept in the in-memory projection for a cleared row and is never mutated in the DB — clearing is `cleared_at` metadata only, so loading the same session twice yields byte-identical messages.

10. **`buildRegistry` always builds a live stack**: `buildRegistry` calls `builtin.BuildStack`, which creates a fresh `tool.NewRegistry()` and populates it locally. The returned `*builtin.Stack` MUST be closed by the caller.

11. **Factory resource ownership transfers only on success**: after `newLLMClient` succeeds, `Factory.Create` owns that client through message reload and the entire build. Any error closes it; only a successfully returned `Service` takes ownership and later closes it through `Service.Close`. This is the failure-side counterpart of daemon's `Session.Close exactly once` contract.

12. **Direct skill invocation is position-sensitive and case-sensitive**: after optional leading Unicode whitespace, only exact `/skill` followed by whitespace is intercepted. Later prose, `/skillful`, `/skill:name`, and differently cased commands remain ordinary user text. User invocation checks only `user-invocable`; model invocation checks only `disable-model-invocation`.

13. **Suspension crosses the session boundary as data**: the private `run` returns the loop's `loopResult`, and `RunDaemon` copies its `Suspended` discriminator into `RunResult`. Daemon must not reconstruct that outcome by scanning producer ledgers after the run: a task or sleep ledger may have been created during the same loop, after the staged-call view used to construct the session.

14. **An external delivery receipt distinguishes acceptance from application**: idempotent schedule injection/reset returns `applied=true` only for the transaction that created the delivery claim and transcript mutation. A same-payload retry is successful with `applied=false`; daemon does not run the model for it and schedule does not republish `NotifyInputReceived`. A mismatched payload for the same identity is an error, never a second application.


<a id="pkg-llm"></a>

## `internal/llm`

_Provider drivers (client construction + model catalogs), retry, cost tracking, reasoning effort._


### Purpose

This package provides a unified `Client` interface for calling LLM chat completion APIs from the agent's ReAct loop, and a private `driverProtocol` interface that owns one provider protocol end to end. Four drivers exist — `anthropic` (native SDK), `openai` (raw HTTP, OpenAI-compatible), `openrouter` (OpenAI-compatible plus provider routing and UI session grouping), `google-sa` (OpenAI-compatible with OAuth2 token refresh) — held in a package-level registry keyed by the provider's `driver` string. Each driver answers two questions: how to construct a client (`NewClient`) and where its models' characteristics come from (`ListModels`). Adding a driver means implementing both; the compiler will not let a new one skip its catalog ([ADR-0003](adr/0003-external-model-catalogs.md)).

Every client returned by the public constructors is wrapped in a bounded-retry decorator (6 attempts, each capped by `defaultModelTimeout`) with exponential backoff, making transient failures invisible to callers. Provider-specific behaviors (DeepSeek thinking tags, Anthropic cache markers via OpenRouter, reasoning effort, OpenRouter provider routing) are handled transparently within the OpenAI-compatible layer.

`EnrichCatalog(ctx, cfg)` runs once at startup from `main`, before the daemon exists: it groups configured models by provider, calls each driver's `ListModels` exactly once, writes the resolved metadata onto `config.ModelEntry` in place, and fails if any model cannot be resolved. Everything downstream (`session/prompt`, `daemon/controller`, the clients themselves) keeps reading the same struct fields — enrichment adds no query surface below `main`.

`Chat` takes `tools []llmwire.ToolSchema`, not `[]tool.Tool` — `llm` has zero dependency on `tool`. Callers convert via `tool.ToSchemas(registry.List())` before calling in. Its only modeled dependency is `catalog`.

### Key Types

| Name | Exported | Role | Owns |
|------|----------|------|------|
| `Client` | yes (interface) | Contract for all LLM backends: `Chat`, `Model`, `APIKey`, `Close`, `Provider`, `ContextWindow`, `SetReasoningLevel`/`GetReasoningLevel`, `SetSessionID` (OpenRouter UI grouping; no-op on non-OpenRouter clients) | nothing — pure interface |
| `driverProtocol` | no (interface) | One provider protocol end to end: `NewClient(entry, model)` + `ListModels(ctx, providerKey, entry)` | nothing — pure interface |
| `anthropicDriver` / `openAIDriver` / `googleSADriver` / `openRouterDriver` | no (structs) | The four `driverProtocol` implementations | a shared `catalog.Fetcher` |
| `thinkingParams` | no (struct) | The reasoning shape one request carries: adaptive thinking + effort, or a token budget | nothing — value type |
| `anthropicClient` | no (struct) | Anthropic native SDK wrapper | `anthropic.Client`, `Usage` accumulator |
| `openaiClient` | no (struct) | Shared base for all OpenAI-compatible backends | `http.Client` (120s timeout), optional `oauth2.TokenSource`, optional `*config.OpenRouterConfig` |
| `openAICompatibleClient` | no (struct, embeds `openaiClient`) | Provider-aware OpenAI-compat layer | Provider detection flags (`isDeepSeek`, `isAnthropic`, `isOpenRouter`), provider string (`"openrouter"`, `"deepseek"`, `"glm"`, `"openai-compatible"`), `sessionID` (set via `SetSessionID`, only honored when `isOpenRouter`) |
| `retryableClient` | no (struct) | Decorator: bounded retry (6 attempts) with exponential backoff + jitter | Wraps any `Client` as `inner` |
| `Usage` | yes (struct) | Token counts + cost for a single API call | nothing — value type |
| `ReasoningLevel` | yes (string type) | The effort vocabulary (none/minimal/low/medium/high/xhigh/max); which values a model accepts is a catalog fact, not a constant | nothing — value type |
| `anthropicParams` | no (struct) | Constructor params for Anthropic client: API key + the catalog-enriched `config.ModelEntry` | nothing — value type |
| `openAICompatibleParams` | no (struct) | Constructor params for OpenAI-compat client: base URL, key/token source, the enriched `config.ModelEntry`, `IsOpenRouter` flag | nothing — value type |
| `anthropicThinkingBlock` | no (struct) | Our own wire shape for a thinking / redacted-thinking block, so a stored payload replays across SDK churn | nothing — serialization only |
| `oaiRequest` / `oaiResponse` / `oaiMessage` / `oaiProvider` / etc. | no (structs) | Wire types for OpenAI-compatible JSON protocol | nothing — serialization only |

### File Map

- **`llm.go`** — `Client` interface, `NewClient`/`NewClientWithModel`/`newClientForModel` (registry dispatch), `findModel`
- **`driver.go`** — private `driverProtocol` interface, driver-name constants, the four implementations, `newDrivers`/`defaultDrivers` registry, models.dev section selection
- **`catalog_modelsdev.go`** — where the models.dev catalog lives (`modelsDevURL`, `modelsDevSource`) and how to read it: wire types, `parseModelsDev`, `reasoning_options` → `config.ReasoningSpec` (including the effort allowlist)
- **`catalog_openrouter.go`** — where the OpenRouter catalog lives (`openRouterSource(baseURL)`, derived from the provider's `base_url`) and how to read it: wire types, `parseOpenRouter`, per-token string prices × 1e6, the per-model `reasoning` object → `config.ReasoningSpec`
- **`enrich.go`** — `EnrichCatalog` → `enrichProvider` (one `ListModels` per provider) → `applySpec` → `validateEnriched` (context window always, max output tokens wherever an Anthropic backend serves) → one info log line per model. A bare `openai` provider flattens all models.dev sections in sorted order, so `warnAmbiguousSection` emits one `catalog_section_ambiguous` warning per configured id that several sections carry — naming the winner, the losers, and the `catalog:` key that pins it — without changing the resolution. `applySpec` also sets `EffortLevels` and `DefaultEffort` via `effortLevels`/`defaultEffort`: the catalog's own allowlist narrowed to what the driver can deliver (`anthropic` native effort → the catalog list, `anthropic` budget models → the three levels the budget mapping covers, `openrouter` → its per-model list, every other driver → none), so the picker never offers a dead control or a level the model would silently remap
- **`anthropic.go`** — `anthropicClient` implementation using the official Anthropic Go SDK (streaming), `addAnthropicNativeCacheMarkers`/`setMessageCacheControl` helpers for prompt caching, `buildMessageParams`/`convertMessages` (role-constant-based, no inline literals)
- **`openai.go`** — `openAICompatibleClient` constructor + `Chat` with provider-specific branching (DeepSeek, Anthropic-via-OpenRouter, OpenAI-via-OpenRouter, OpenRouter session grouping), `addAnthropicCacheMarkers`/`applyCacheControl` helpers, `openAISuffix` constant for enforcing native function calling
- **`openai_http.go`** — `openaiClient` shared base: HTTP request execution, message/tool conversion, response logging
- **`openai_parse.go`** — Response parsing: `parseMessage`, `extractUsage`, `extractThinkContent`, `removeThinkTags`, `ensureSchemaProperties`
- **`openai_types.go`** — Wire types (`oaiRequest`, `oaiResponse`, `oaiMessage`, `oaiProvider`, etc.), `openaiClient` struct definition + `newOpenAIClient` constructor (clamps `maxTokens` to the context-window output reserve), `oaiMessage.content()` method (handles both string and content-block-array response formats)
- **`toolschema.go`** — `parseParamsSchema` — unmarshals a `llmwire.ToolSchema.Parameters` blob into a `map[string]any` for SDK-specific request assembly
- **`google_auth.go`** — `newGoogleTokenSource`: reads a Google SA JSON key file, returns an auto-refreshing `oauth2.TokenSource`
- **`retry.go`** — `retryableClient` decorator: bounded retry loop (6 attempts), string-based error classification, exponential backoff with jitter
- **`pricing.go`** — `estimateCost(usage, *config.ModelPricing)`: the catalog-resolved rates and nothing else. A model whose catalog carries no cost is billed at zero — an honest zero beats a sonnet-class guess
- **`reasoning_envelope.go`** — the `{model, payload}` envelope stamping a provider's reasoning payload with its producing model, and `wrapReasoning`/`unwrapReasoning`. Unwrapping succeeds only for the model that produced the payload, so replay is never cross-model. This package is the only one that opens the envelope — everyone between here and SQLite carries it sealed
- **`anthropic_thinking.go`** — everything reasoning on the native path: `buildThinkingParams(spec, level, maxTokens)` (the pure mapping from a catalog `ReasoningSpec` + effort level onto the request's thinking/effort shape) with its budget clamp, the `anthropicThinkingBlock` wire type, and `replayThinkingBlocks` (model-matched round-trip)
- **`validate.go`** — `ValidateToolPairing(messages []llmwire.Message) error`: enforces the tool_use/tool_result pairing invariant `convertMessages` relies on (every assistant tool_call has a matching result) -- the test oracle for transcript validity, usable as a defensive assert before `Chat`

### State Ownership

**`openaiClient.httpClient`** — A `*http.Client` with 120s timeout. Created once per client in `newOpenAIClient`. Not shared across clients. No mutex needed (Go's `http.Client` is goroutine-safe).

**`openaiClient.tokenSource`** — An `oauth2.TokenSource` from Google's OAuth2 library. Tokens are refreshed lazily on each `buildHTTPRequest` call. The `oauth2` library handles its own internal locking for token caching/refresh. Source of truth: Google's token endpoint.

**`anthropicClient.usage`** — A `*Usage` pointer allocated in the constructor but never actually accumulated across calls. Each `Chat` call creates a fresh local `Usage`. The field is effectively dead state.

**`openaiClient.openrouterConfig`** — An optional `*config.OpenRouterConfig` set at construction time. Read-only after construction. Used by `openAICompatibleClient.Chat` to attach OpenRouter provider routing (`only`/`order` fields) to the request body. Nil for non-OpenRouter providers.

**`openaiClient.pricing` / `anthropicClient.pricing`** — The model's catalog-resolved `*config.ModelPricing`, copied at construction. Read-only. Used by `estimateCost` when the provider doesn't return cost directly; nil bills the call at zero.

**`openaiClient.reasoning` / `anthropicClient.reasoning`** — The model's catalog-resolved `*config.ReasoningSpec`, copied at construction. Read-only. It is the whole gate on whether a request carries reasoning configuration at all, replacing the old `openai/`-prefix heuristic.

**`openaiClient.replayReasoning`** — Set true only for the OpenRouter driver. Gates echoing a stored `reasoning_details` array back on assistant messages; an arbitrary OpenAI-compatible endpoint can 400 on the unknown field.

**`reasoningLevel`** — Stored on each concrete client (`anthropicClient`, `openaiClient`). Mutable via `SetReasoningLevel`. No synchronization. Read during request construction (`Chat`). Default is `"medium"` (returned when field is empty string).

**`defaultDrivers`** — Package-level `map[string]driverProtocol` built once at init from a single shared `catalog.Fetcher`, so models.dev is fetched at most once per process however many providers reference it — the memo is keyed by URL, so the three drivers reading it coalesce. Never mutated. Tests build their own registry via `newDrivers(fetcher)`.

There are no hardcoded per-model tables left — no context windows, no prices, no budget numbers. Every one of those values arrives from the catalog and is validated at startup.

### Concurrency Model

This package has minimal concurrency concerns. Each `Client` instance is owned by a single session and called sequentially from the agent's ReAct loop.

**Single goroutine per request**: `executeHTTPRequest` spawns a progress-logging goroutine that ticks every 10 seconds until the HTTP response arrives or context cancels. Synchronization is via a `done` channel (closed when response arrives). The goroutine reads `ctx.Deadline()` which is immutable.

**No mutexes**: None of the client structs use mutexes. `reasoningLevel` is read/written without synchronization, which is safe only because each client is used from a single goroutine (the session's agent loop).

**Retry loop**: `retryableClient.Chat` runs a bounded `for` loop (`maxRetryAttempts = 6`) with `time.After` delays, respecting `ctx.Done()`. No goroutines are spawned — retry is synchronous from the caller's perspective.

### Data Flow

#### Standard Chat Request (OpenAI-compatible path)

```
caller (agent loop)
  -> retryableClient.Chat()
    -> openAICompatibleClient.Chat()
      -> openaiClient.convertMessages()     // llmwire.Message -> []map[string]any
      -> openaiClient.convertTools()         // tool.Tool -> []oaiToolDef
      -> apply OpenRouter provider config    // oaiProvider{Only, Order} if openrouterConfig set
      -> attach sessionID                    // if isOpenRouter && sessionID set: reqBody.SessionID (UI grouping)
      -> system prompt injection (provider-specific: DeepSeek=user msg, Anthropic=cache_control blocks, default=system role)
      -> addAnthropicCacheMarkers()          // if isAnthropic: up to 3 cache breakpoints
      -> enable thinking/reasoning           // DeepSeek=chat_template_kwargs, OpenAI=reasoning.effort
      -> openaiClient.makeRequest()
        -> openaiClient.buildHTTPRequest()   // auth header, optional OAuth2 token refresh
        -> openaiClient.executeHTTPRequest() // HTTP POST, progress logging goroutine
        -> json.Unmarshal -> oaiResponse
        -> openaiClient.parseMessage()       // oaiMessage -> llmwire.Response (handles <think> tags)
        -> extractUsage()                    // oaiResponse.Usage -> Response.Usage/CostUSD
    <- (*llmwire.Response, error)
  <- retry on transient error, or return
```

#### Standard Chat Request (Anthropic path)

```
caller (agent loop)
  -> retryableClient.Chat()
    -> anthropicClient.Chat()
      -> buildMessageParams()/convertMessages()  // llmwire.Message -> anthropic.MessageParam via roleUser/roleAssistant/roleTool constants
      -> addAnthropicNativeCacheMarkers()    // up to 3 cache breakpoints (system, first user, sliding window)
      -> convert tools via anthropicClient.convertTools()
      -> anthropic.Client.Messages.NewStreaming()  // streaming required for Opus
      -> Accumulate loop + manual delta usage copy // SDK Accumulate drops cache tokens from message_delta
      -> anthropicClient.parseResponse()     // anthropic.Message -> llmwire.Response
      -> estimateCost()                      // Usage + pricing -> Response.CostUSD (hardcoded Anthropic prices)
    <- (*llmwire.Response, error)
  <- retry on transient error, or return
```

#### Client Construction

```
config.Config
  -> NewClient() / NewClientWithModel()
    -> find model in UnifiedConfig.Models
    -> look up model's provider in UnifiedConfig.Providers
    -> dispatch on provider.Driver: anthropic | openai | google-sa | openrouter
    -> construct inner Client (anthropicClient | openAICompatibleClient, IsOpenRouter:true for the openrouter driver)
    -> newRetryableClient(inner)
  <- Client (retryableClient wrapping the chosen backend)
```

`NewClientWithModel` only overrides the *model* (falling back to `NewClient`/the default model when empty) — there is no subagent-specific credential override. Subagent model selection is a flat `config.Config.SubagentModel` string resolved by the caller (`daemon`/`session`) before construction; provider/driver lookup is always the standard per-model path.

### Lifecycle

#### Client

```
[Created] -- NewClient/NewClientWithModel --> [Ready] -- Chat --> [Ready] (stateless per call)
                                                  |
                                                Close() --> [Closed] (no-op for all implementations)
```

All client implementations have no-op `Close()`. There is no connection pooling, no persistent state to clean up. The `http.Client` has no `CloseIdleConnections` call. The Anthropic SDK client has no teardown.

#### Retry Loop

```
[attempt 0] -- call inner --> success: return
                          --> error + retryable: calculate delay -> sleep (or ctx cancel) -> [attempt N+1]
                          --> error + non-retryable: return error immediately
```

The retry loop is bounded: 6 attempts (`maxRetryAttempts`), each attempt capped by `defaultModelTimeout` (10 min). It stops early on context cancellation or a non-retryable error. The backoff caps at `llmRetryMaxDelay`.

### Tensions

1. **Dead `usage` field on `anthropicClient`**: The constructor allocates `usage: &Usage{}` and stores it as a struct field, but `Chat` creates a fresh local `Usage` per call and never reads or writes the field. This suggests an earlier accumulation design that was replaced with per-call returns but the field was not removed.

2. **String-based error classification**: `shouldRetry` matches error strings with `strings.Contains` on status codes ("500", "429") and phrases ("bad gateway"). This means any error message containing the substring "500" (e.g., a model ID containing "500") would be classified as a server error. The Anthropic SDK returns typed errors that are not leveraged.

3. **Provider detection by model name prefix**: `openAICompatibleClient` still determines cache-marker and DeepSeek behavior (`isDeepSeek`, `isAnthropic`) and the provider string (`"openrouter"`, `"deepseek"`, `"glm"`, `"openai-compatible"`) from model-name prefixes at construction. A model named `"anthropic/custom-finetune"` would get Anthropic cache markers regardless of what the backend supports. Reasoning is no longer in this list — it moved to the catalog's capability flag.

4. **Two reasoning channels per message**: `ReasoningContent` (plain text, for display/telemetry) and `ReasoningRaw` (the provider's own payload, for wire fidelity) both live on an assistant message and are set independently. They are not derived from one another, and only `ReasoningRaw` is replayed.

### Ordering Constraints

**Enrichment before any client construction**: `EnrichCatalog` runs in `main` immediately after `config.NewConfig()` and before the daemon exists. Every client constructor reads limits, pricing and reasoning capability straight off `config.ModelEntry`, so a client built before enrichment would silently carry zeros. (Why the fatality is at startup and not at client build: a model that cannot be served should fail once, loudly, not on the user's first message.)

**Provider/driver dispatch in `newClientForModel`**: Model → provider → driver registry → `driver.NewClient`. No priority logic — the model's provider is explicit in config, and the provider's driver names the registry entry.

**Thinking blocks lead an assistant message**: `buildAssistantBlocks` emits replayed thinking blocks before text and tool_use, matching the order the model produced them. (Why: the API rejects a thinking block that appears after content in the same message.)

**System prompt injection before cache markers**: In `openAICompatibleClient.Chat`, the system prompt is prepended to messages before `addAnthropicCacheMarkers` runs. (Why: cache markers index into the message slice; inserting the system prompt after marking would shift indices and mark the wrong messages.)

**`shouldRetry` clause ordering**: Billing/credits exhaustion (402, `"credits"`, `"insufficient"`) is checked first as non-retryable — no point retrying until the user tops up. Then quota/limit checks (`"key limit"`, `"quota"`, `"limit exceeded"`) must come before the general 403 check. (Why: quota errors often arrive as 403 responses. If the 403 check ran first, quota exhaustion would be treated as non-retryable auth failure.) Then auth (401/403), then bad request (400), then retryable server/network errors.

### Package Anti-patterns

**Don't reintroduce a per-model table**: no context windows, no prices, no budget numbers, not even as a fallback. Startup validation guarantees `ContextWindow()` is non-zero for every configured model; a second source would recreate exactly the drift ADR-0003 removed.

**Don't replay a reasoning payload across models**: `unwrapReasoning` returns the payload only when the envelope's model equals the current one. Loosening that check would send another model's signed thinking blocks and 400 the request.

**Don't set `reasoningLevel` from a goroutine other than the agent loop**: The field has no synchronization. It is safe only because the session architecture guarantees single-goroutine access per client. Sharing a client across goroutines would race on this field.

**Don't let an Anthropic-backed model through with `MaxTokens == 0`**: both `newAnthropicClient` and `newOpenAICompatibleClient` (for `anthropic/`-prefixed ids) error out. Elsewhere 0 means "let the provider decide" (`max_tokens` is `omitempty`); Anthropic's API mandates an explicit value. `validateEnriched` mirrors the same two conditions so the failure lands at startup instead of mid-session.

**Don't send the catalog's max output tokens verbatim**: `newOpenAIClient` clamps it to the **output reserve** — `min(catalog max, (1 − llmwire.ContextInputFraction) × ContextWindow)`, the complement of `session`'s compaction threshold. Providers enforce `input + max_tokens ≤ window`, and OpenRouter reports `max_completion_tokens == context_length` for a class of models (kimi-k2.5, glm-5), so the raw value 400s every request by construction. The two invariants compose without any client-side input estimation: compaction keeps input ≤ 85%, the clamp keeps output ≤ 15% ([ADR-0010](docs/adr/0010-output-budget-clamps-max-tokens.md)). The clamp bounds a value we already send — `MaxTokens == 0` stays 0 (omitted), and a window too small to carry a reserve is left alone. The native Anthropic client is deliberately out of scope, and the catalog fields themselves stay honest: `ModelSpec`/`ModelEntry` and the `model_enriched` startup log report the provider's number, not the clamped one.

### Provider-Specific Behaviors

The `openAICompatibleClient` applies these provider-specific transformations in `Chat`:

| Behavior | DeepSeek (`isDeepSeek`) | Anthropic via OpenRouter (`isAnthropic`) | Other OpenRouter models | Default |
|----------|------------------------|------------------------------------------|-------------------------------------|---------|
| System prompt | Injected as user+assistant message pair (workaround: DeepSeek breaks function calling with system role) | System message with `cache_control` content blocks | Standard `system` role message | Standard `system` role message |
| Function call enforcement | `openAISuffix` appended to prompt | `openAISuffix` appended to prompt | `openAISuffix` appended to prompt | `openAISuffix` appended to prompt |
| Cache markers | — | Up to 3 `cache_control` breakpoints (system, first user, sliding window) | — | — |
| Thinking/reasoning | `chat_template_kwargs: {thinking: true}` | `reasoning: {effort: <level>}`, clamped to the model's allowlist; omitted entirely when the catalog declares no effort selector | same as OpenRouter | — (an arbitrary endpoint can 400 on the field) |
| `reasoning_details` echo | — | Prior turn's payload replayed verbatim | Prior turn's payload replayed verbatim | — |
| Provider routing | `oaiProvider` from `openrouterConfig` if set | `oaiProvider` from `openrouterConfig` if set | `oaiProvider` from `openrouterConfig` if set | `oaiProvider` from `openrouterConfig` if set |

### Contracts

**Retry wrapping is mandatory for public construction**: `NewClient` and `NewClientWithModel` always wrap the inner client in private `newRetryableClient`. Direct construction of `anthropicClient` or `openAICompatibleClient` bypasses retry. Any new factory function must maintain this wrapping. (Why: the agent loop has no retry logic of its own; it assumes the client handles transient failures.)

**`llmwire.Response.FinishType` signals tool calls**: When the response contains tool calls, `FinishType` is set to `"tool_calls"` (constant `finishTypeToolCalls`). When it contains only text, it defaults to `"stop"`. The agent loop uses this to decide whether to execute tools or return. (Why: mismatched finish types cause the agent to either ignore tool calls or fail to terminate.)

**Cost is computed at the client level, not the caller**: Both Anthropic and OpenAI paths populate `llmwire.Response.CostUSD` and `llmwire.Response.Usage` before returning. The caller never needs to compute cost independently. For OpenAI-compatible providers, if the API returns a `cost` field in usage (e.g., OpenRouter), it is used directly; otherwise `estimateCost` is called. (Why: cost computation requires provider-specific knowledge of pricing tiers and cache semantics.)

**Provider string determines pricing lookup key**: `estimateCost` builds a lookup key as `"provider/model"` (lowercased). The `provider` value comes from `Provider()` on the concrete client (`"anthropic"`, `"openrouter"`, `"deepseek"`, `"glm"`, `"openai-compatible"`). Changing a client's `provider` string changes its pricing lookup path. (Why: the hardcoded price table and config override both key on this composite string.)

**`openAICompatibleClient` embeds `openaiClient` — method resolution matters**: `openAICompatibleClient` overrides `Chat`, `Provider`, and `SetSessionID` but inherits `Model`, `APIKey`, `Close`, `ContextWindow`, `SetReasoningLevel`, `GetReasoningLevel`, `convertMessages`, `convertTools`, `makeRequest`, `buildHTTPRequest`, and `executeHTTPRequest` from the embedded `openaiClient`. Adding a method to `openaiClient` automatically exposes it on `openAICompatibleClient`. (Why: forgetting this when adding provider-specific behavior leads to the base implementation silently handling calls it shouldn't.)


<a id="pkg-catalog"></a>

## `internal/catalog`

_Catalog transport: HTTP fetch, disk cache, id matching, effort vocabulary. Endpoints and formats belong to the drivers._


### Purpose

Transport and vocabulary, nothing more. **This package names no catalog and knows no payload format.** A driver hands it a `Source` — URL, cache filename, and a `Validate` function that is the driver's own parser — and gets a raw body back. Endpoints, formats and section choices all live with the driver that owns them ([ADR-0003](adr/0003-external-model-catalogs.md), [ADR-0006](adr/0006-catalog-transport-owns-no-endpoint.md)); adding a driver with a brand-new catalog does not touch this package at all. It is a leaf — it imports `config` (for `ModelPricing`/`ReasoningSpec`) and `logger`, nothing else.

Validation runs on the driver's behalf in both directions, which is what keeps the transport format-blind without losing the parse-before-write guarantee: a body the driver's parser rejects is neither cached nor returned.

### Key types

| Name | Type | Role |
|------|------|------|
| `Source` | struct | Where one catalog lives and how to vet it: `URL`, `CacheName`, `Validate func([]byte) error`. Every field is the driver's |
| `Fetcher` | interface | `Fetch(ctx, Source) ([]byte, error)` — the whole surface |
| `fetcher` | struct | Implementation: HTTP client, cache dir, and a per-URL memo so drivers sharing a catalog fetch it once |
| `Option` | func type | `WithHTTPClient` / `WithCacheDir` — the seams tests use to stay off the network |
| `ModelSpec` | struct | One model's resolved metadata: `Name`, `Source` (which section it came from), `ContextWindow`, `MaxTokens`, `Pricing`, `Reasoning`, `Shadowed` (sections carrying the same id that lost a `Flatten`) |

### File map

- `catalog.go` — `ModelSpec`, `Lookup` (one section), `Flatten` (merge every section into one id-keyed map, sorted order — an id several sections carry resolves to the alphabetically-first one, and the losers are recorded on `ModelSpec.Shadowed` for the caller to report), `normalizeID`
- `fetch.go` — `Source`, `Fetcher`/`fetcher`, options, `New`, `CacheName`, the fetch-validate-cache-or-fall-back-to-cache flow, cache paths
- `effort.go` — the effort vocabulary shared by both catalog formats: `SortEfforts` (canonical weakest-first), `ClampEffort`, and their private rank helper

### State ownership

| State | Location | Source of truth | Synchronization |
|-------|----------|-----------------|-----------------|
| Fetched bodies (and their errors) | `fetcher.memo`, keyed by URL | The remote catalog, or its disk snapshot | `sync.Mutex` for slot creation, `sync.Once` per slot |
| Raw response snapshots | `~/.coagent/cache/catalog/` | Disk | none (last writer wins; a stale snapshot is still a valid one) |

No background refresh: the lifecycle is fetch-once-at-startup, and refreshing means restarting the daemon. An unresolvable home directory disables the cache rather than failing — the network path still works.

### Ordering constraints

**Parse before write.** The cache is written only after the body parses successfully. A 200 carrying an error page is a fetch failure and must not clobber a good snapshot.

**Fetch, then fall back.** On fetch *or* parse failure the cached copy is read back at any age, with a warn. No cache either → the error names the URL and says it was not cached, and the affected models fail `EnrichCatalog`'s validation.

**Exact match, then normalized.** `Lookup` tries the configured id verbatim first, then strips a trailing `-YYYYMMDD`/`@YYYYMMDD` from **both** sides and compares again — models.dev's google-vertex keys are the dated side, anthropic config ids may be. Ties resolve in sorted key order so the answer does not change between restarts. No fuzzier matching.

**Sorted sections when flattening.** `Flatten` visits sections in sorted order and never overwrites an id already claimed, so a duplicated id always resolves to the same section. This is what makes a bare `openai` provider's cross-section search deterministic.

### Package anti-patterns

**Don't add a TTL refresh loop.** Model catalogs change slower than the daemon restarts; a refresh goroutine is a moving part with no payoff (ADR-0003).

**Don't let a parse failure look like success.** Both parsers (in `llm`, wired in as a `Source.Validate`) reject an empty payload (`{}`, `{"data": []}`) — a well-formed but empty catalog is indistinguishable from a broken upstream and would silently unresolve every model.

**Don't name a catalog in this package.** No endpoint constants, no format-specific method on `Fetcher`, no parser. The moment a URL or a payload shape appears here, adding a driver means editing shared code again — the exact coupling ADR-0006 removed.

<a id="pkg-tool"></a>

## `internal/tool`

_Pure protocol leaf: `Tool`/`Result`/`Registry` contract, `ErrSuspend`, tool-ID constants, call-ID context, `SleepParams`/`ParseDuration`, truncation budget. No implementations, no cross-package DI interfaces._


### Cross-cutting insight

The `Tool` interface's `Description()` and `Parameters()` methods are part of the LLM prompt cache key. Non-deterministic output from these methods (e.g., iterating maps) silently destroys cache hit rates across the entire system. This constraint is enforced by convention, not by the type system. Every package that defines a tool (`daemon`, `schedule`, `session`, `tool/builtin`) has an explicit dependency on the protocol package, but none depends on another tool-producing package through it.

---

### 1. Purpose

This package defines the tool abstraction used by the agent's ReAct loop and provides a thread-safe tool registry. As of the July 2026 split, it owns **only the protocol** — `Tool`/`Result`/`Registry`, the `ErrSuspend` sentinel, call-ID context propagation, tool-ID constants, `SleepParams`/`ParseDuration`, and the tool-result truncation budget calculation. It has **no tool implementations** (those live in `tool/builtin`) and **no cross-package DI interfaces** (the former `Spawner`/`ScheduleStore`/`Compactor`/`CuratedMemoryStore` are gone — see [Interface boundaries](#interface-boundaries)). Every package that implements a tool needing domain state (daemon, schedule, session) now depends directly on that state or a tiny private interface scoped to itself, not on an interface exported from here.

### 2. Key types

| Name | Exported | Role | What it owns |
|---|---|---|---|
| `Tool` | yes (interface) | Contract for all tools: ID, Description, Parameters, Execute | Nothing — pure interface |
| `Result` | yes (struct) | Uniform tool output: Title, Output text, Metadata map | The data returned from one execution |
| `Registry` | yes (interface) | Tool collection with lookup, execution dispatch, cloning, filtering | Nothing — pure interface |
| `RegistryBound` | yes (interface) | `Tool` + `BindRegistry(Registry) Tool`, implemented by tools that dispatch through a registry (`batch`); `Clone`/`Filter` rebind them onto the derived view | Nothing — pure interface |
| `svc` | no (struct) | Registry implementation | `mu sync.RWMutex`, `tools map[string]Tool` |
| `ErrSuspend` | yes (sentinel error) | Signals the agent loop to checkpoint and exit without recording a tool result | N/A |
| `SleepParams` | yes (struct) | Wire contract for the `sleep` tool (`Duration`, `Reason`); kept here (not in `schedule`, which owns the tool) so the session's resume path can re-parse the args too | N/A (data struct) |
| `ID*` constants | yes | `IDSleep`, `IDSchedule`, `IDTask`, `IDCompactContext`, `IDBatch`, `IDSkill` — well-known tool IDs referenced across packages for session resume and daemon injection | N/A |

### 3. File map

| File | What it contains |
|---|---|
| `tool.go` | `Tool` interface, `RegistryBound` interface, `Result` struct, `Registry` interface, `svc` registry impl (incl. the `bind` rebinding step), `NewRegistry()`, `ErrSuspend`, `ID*` constants, `ToSchemas(tools []Tool) []llmwire.ToolSchema` |
| `context.go` | `WithCallID(ctx, callID)` / `CallIDFromContext(ctx)` — propagates the in-flight tool_call id through context, set per-goroutine in the session's `executeToolCall` so concurrent calls never share an id |
| `sleep_duration.go` | `SleepParams`, `ParseDuration()` (Go duration / human `Nd`/`Nw` units / RFC3339 timestamp), `parseHumanDuration()` |
| `truncate.go` | `MaxToolResultSize`, `MaxToolResultContextShare`, `DynamicToolResultBudgetForWindow()` — tool result truncation budget calculation |

### 4. State ownership

| State | Location | Source of truth | Synchronization | Failure behavior |
|---|---|---|---|---|
| Tool map | `svc.tools` | Registry instance | `svc.mu` (RWMutex) | Registry methods return nil/false for missing tools |

### 5. Concurrency model

**Registry (`svc`)**: Uses `sync.RWMutex` to protect the `tools` map. Read operations (`Get`, `List`, `IDs`) take read locks; mutations (`Register`, `Unregister`) take write locks. `Clone()` and `Filter()` create independent registries that share the underlying `Tool` implementation pointers (shallow copy) — except for `RegistryBound` tools, which are rebound onto the new registry so a derived view can never dispatch into the tool set it was derived from. No goroutines are spawned by this package at all -- it is pure data structure + helper functions.

### 6. Data flow

#### Tool dispatch (generic, any tool)
```
Agent loop -> Registry.Execute(ctx, id, params)
  -> svc.Get(id) -> Tool
  -> tool.Execute(ctx, params) -> (*Result, error)
```

#### Sleep/suspend sentinel (mechanism only -- the `sleep` tool itself lives in `schedule`)
```
Any tool -> return nil, ErrSuspend
Agent loop (session) catches ErrSuspend -> checkpoints session, exits loop without recording a result
Daemon injects the real result on resume
```

### 7. Lifecycle

**Registry**: Created empty via `NewRegistry()`. Tools are registered individually by `builtin.BuildStack`, the daemon (`registerScheduleTools`/`registerSubagentTools`), and session setup. The registry can be cloned (`Clone()`) for subagents or filtered (`Filter()`) to restrict available tools. No shutdown protocol.

### 8. Tensions

1. **`Description()` determinism is convention, not enforced.** The `Tool` interface comment warns that `Description()` and `Parameters()` must be deterministic for prompt caching. Enforcement is entirely by review discipline across every package that now defines a tool (daemon, schedule, session, `tool/builtin`) -- this package has no way to check it.

### 9. Ordering constraints

1. **Registry population before `BatchTool` use** (in `tool/builtin`): All tools that might be batch-invoked must be registered before the agent starts. `BatchTool` pre-validates all tool IDs via `registry.Get()` before executing any -- a missing tool fails the entire batch. It validates against the registry it was served from (contract 6): tools the daemon later registers onto a session's filtered registry become batchable, tools the filter removed never do.

### 10. Package anti-patterns

1. **Don't register the same tool ID twice**: `Registry.Register()` silently overwrites. If two tools share an ID, the second one wins with no warning.

2. **Don't return `ErrSuspend` from a tool that hasn't persisted a wake-up mechanism first**: the `sleep`/`schedule` tools (in `schedule`) persist a schedule row before returning `ErrSuspend`. Any tool returning `ErrSuspend` without an external wake-up path causes the session to suspend permanently.

3. **Don't add a cross-package DI interface here.** The July split deliberately removed `Spawner`/`ScheduleStore`/`Compactor`/`CuratedMemoryStore`. A tool that needs domain state belongs in the package that owns that state (see the daemon/schedule/session sections), not bolted onto this leaf via a new interface.

4. **Don't iterate `svc.tools` without sorting**: The `tools` field is a `map[string]Tool`. Direct iteration produces non-deterministic order. `List()` and `IDs()` sort their output explicitly. Any new method that iterates the map must also sort for prompt cache stability.

### 11. Contracts

1. **ErrSuspend implies no Result**: When a tool returns `(nil, ErrSuspend)`, the agent loop must not record a tool_result message. The result is injected later by the scheduler/daemon. If both a Result and ErrSuspend are returned, behavior is undefined — the current implementation only checks for ErrSuspend.

2. **Registry.Clone() is shallow**: Cloned registries share the same `Tool` implementation pointers. Tools that hold mutable state will reflect mutations across all clones. This is intentional but means a cloned registry is not fully independent.
 The one exception is `RegistryBound` tools — see contract 6.

3. **`ToSchemas` order is part of the prompt-cache key**: `ToSchemas(tools []Tool) []llmwire.ToolSchema` preserves input order — callers must pass an already-sorted tool list (`Registry.List()` sorts by ID).

4. **Tool result budget is context-window-aware**: `DynamicToolResultBudgetForWindow()` computes a truncation budget as the lesser of `MaxToolResultSize` (25000 chars) and 30% of the context window (in chars, approximated as 4 chars/token). Used by the session layer to truncate oversized tool outputs.

5. **`SleepParams` lives here, not in `schedule`**: so that `session`'s resume path can re-parse a pending sleep tool_call's args without importing `schedule`.

6. **A derived registry is a confinement boundary**: `Clone()`/`Filter()` call `BindRegistry` on every `RegistryBound` tool they carry over, so a dispatching tool resolves and executes callees against the derived view only. Without it the agent-type allowlist would be bypassable through `batch`. Any new tool holding a `Registry` MUST implement `RegistryBound`.


<a id="pkg-shellenv"></a>

## `internal/shellenv`

_Per-directory login+interactive shell snapshot so tool subprocesses run as if a fresh terminal had opened in that directory._

### Purpose and boundary

coagent is a long-lived multi-project daemon; each session's working directory comes from the DB, not from how the daemon launched. Inherited `os.Environ()` is frozen to one directory's toolchain, so per-project version managers (mise/asdf/nvm/direnv/pyenv) resolve wrong. `shellenv` captures a per-cwd snapshot of the user's activated shell state once, caches it, and replays it via `source` for later spawns in that directory.

It is a pure leaf: it imports no non-common internal package (only `logger`). Its consumers are the three per-cwd spawners — `bashsandbox` (bash tool), `lsp` (language servers), `mcp` (stdio servers) — plus `session`/`builtin` for wiring and `main` for construction.

### Mechanism

- **Capture:** `$SHELL -l -i -c '<dump>'` with `Dir=workDir`, stdin `/dev/null` (an interactive login shell otherwise blocks on read/prompt hooks), stderr discarded (interactive-on-non-tty job-control noise), and `cmd.WaitDelay` bounded so an rc-spawned lingering daemon holding the stdout pipe can't wedge the capture past the timeout. The dump prints a marker, then `shopt -p; declare -f; alias -p`, then a second marker, then `export -p`. `shopt -p` precedes `declare -f` because parse-affecting options (extglob/globstar) must be re-enabled before function bodies are read on replay; `export -p` is emitted last behind its own marker so the readonly filter touches only exported-var lines. Output before the first marker (rc banners/motd on stdout) is stripped; in the export section every top-level `declare -<flags>` line whose flags set both `r` and `x` (readonly-exported) is filtered so replay never warns on a re-declared readonly var. The filter is line-oriented, so it cannot distinguish a `declare -…rx…`-shaped continuation line *inside* a multi-line exported value — an accepted, vanishingly rare hole (the section split already confines it to export values, never function bodies).
- **Replay:** bash uses `Snapshot`+`Shell` directly (it composes its own argv around the bwrap wrapper): `<shell> -c "source <snap>; <command>"`. LSP/MCP use `WrapExec`: `<shell> -c "source <snap>; exec <argv>"`, so the server inherits the activated env/PATH but the process is the server, not a shell wrapper. Snapshot path and every argv element are individually single-quoted.

### Public contract

| Name | Role |
|---|---|
| `Provider` | `Snapshot`, `Shell`, `Fingerprint`, `Invalidate`, `WrapExec`, `Close` |
| `Snapshot(ctx, workDir) string` | Returns a valid snapshot path (fingerprint still matches + within the 30-min backstop) or recaptures; `""` when unavailable. **Never returns an error** — logs internally and degrades |
| `Shell() string` | Resolved bash-family shell, or `""` if unsupported |
| `Fingerprint(workDir) string` | Hash of the on-disk state that determines the activated env (toolchain configs, manager state dirs, rc files); `""` when unavailable. Consumed by `mcp` to tie its failed-server retry cooldown to the env |
| `Invalidate(workDir)` | Drops the cached fingerprint so the next `Snapshot` recaptures — a best-effort accelerator for env mutations the daemon observes |
| `WrapExec(ctx, workDir, argv, extraEnv) (*exec.Cmd, error)` | `source <snap>; exec <argv>` with `Dir=workDir`, `Env=os.Environ()+extraEnv`; plain exec of argv with no snapshot. Errors only on empty argv |
| `New() Provider` | Infallible; generates a per-instance salt; cache dir created lazily |

### State ownership

- **Per-instance salt** (random, generated once) — keys the cache as `sha512(workDir+salt)`, so paths aren't predictable across daemons.
- **Snapshot cache** — one file per working directory, validated by a **fingerprint** of the on-disk state that determines the env (walk-up toolchain configs + manager state dirs + rc files, including negative entries), with a 30-minute wall-clock backstop. A change to any controlled file yields a new fingerprint → recapture on the next spawn; the TTL only bounds the un-fingerprintable residue (see [ADR-0001](docs/adr/0001-shellenv-fingerprint-invalidation.md)). An in-memory `workDir→fingerprint` map records the last capture. Concurrent first-spawns for the same key are serialized by a **per-key** lock (not one global lock) so parallel spawns don't stampede N interactive shells.
- **Cache dir** — lazily created `0700` under `os.UserCacheDir()` (bwrap-visible via `--ro-bind / /`, so the replay `source` reaches it). Snapshot files are `0600`. `Close` removes the per-instance dir.

### Invariants

- **Security (hard-to-reverse):** the capture shell inherits `os.Environ()` **only** — never a secrets map. coagent secrets live solely in the in-memory `config.Secrets` map, so they structurally cannot appear in a snapshot. `shellenv` must not import `config` and must have no parameter accepting a secrets map; a footgun comment guards the capture site. `os.Environ()` may still hold operator-exported credentials; those are at-rest-protected by the `0600`/`0700` file modes, not by filtering (full-fidelity stands).
- **Bash-only scope:** snapshotting is supported only when the resolved `$SHELL` basename (symlinks resolved) is `bash`. Any other shell (`/bin/sh`/dash/zsh) or empty `$SHELL` with no `bash` on `PATH` → `Shell()==""` and every consumer spawns exactly as before. zsh is an explicit follow-up.
- **Graceful fallback everywhere:** the provider never returns a fatal error for unavailability. `nil` provider, non-bash shell, capture failure/timeout, or a missing dir all degrade to today's behavior; no consumer propagates a snapshot failure into a spawn failure. Capture runs **unsandboxed**; only the final user command runs under bwrap.
- **Env precedence:** `source` runs after `cmd.Env` is established, so snapshot-exported vars win on name collision (notably `PATH`). Non-colliding MCP `env` vars (server secrets) survive.


<a id="pkg-bashsandbox"></a>

## `internal/bashsandbox`

_Native direct-filesystem-write confinement shared by Bash descendants and dedicated file mutations._

### Purpose and boundary

`bashsandbox` is a leaf execution-policy package. The private `builtin.bashTool` delegates command construction to its `Runner`; the private `builtin.fileMutator` uses the same runner for enabled `write`/`edit`/`apply_patch` mutations. Disabled mode constructs the original `bash -c <script>` command without probing a backend, while file mutations retain their direct Go implementation.

Enabled mode preserves host executables, libraries, filesystem reads, environment, network, and local sockets while limiting direct filesystem writes.

### Threat model

This is a **write-integrity boundary**, not a confidentiality, prompt-injection, or multi-tenant boundary. It limits accidental or malicious direct filesystem mutations outside the writable roots, including mutations attempted by Bash descendants and the dedicated file tools.

It does not protect readable data. Bash and the built-in `read`/`ls`/`glob`/`grep` tools can read any host file available to the daemon user, including credentials and other projects. Network, Unix sockets, and MCP remain unrestricted, so readable data can leave the machine. External MCP/LSP processes are not launched through this runner and may perform their own filesystem writes. Enabling this policy therefore does not make untrusted tasks, untrusted prompt content, or mutually untrusted users safe to run together.

### Public contract

| Name | Role |
|---|---|
| `Config` | `Enabled`, session `WorkDir`, and extra `WritablePaths` |
| `Runner` | `Command(ctx, script, workDir, args...)` — plain `bash -c` for file mutations and the probe (never sources a snapshot); `ShellCommand(ctx, command, workDir)` — user shell commands, sourcing the per-cwd `shellenv` snapshot when available |
| `New(Config, shellenv.Provider)` | Returns the plain runner when disabled; validates roots, probes the backend, and preflights the launcher when enabled. The provider (nilable) is attached to the returned runner, never threaded through the probe factory so `Probe` never snapshots |
| `Probe()` | Verifies once per process that the backend actually confines writes; `main` calls it at startup, `New` lazily |

Writable roots are canonical existing directories. Relative paths, `~user`, missing paths, files, and filesystem root are rejected. Defaults are the session workspace, `os.TempDir()`, `/tmp`, and the existing `os.UserCacheDir()`; additional tool-specific caches are explicit config.

### File map

| File | Responsibility |
|---|---|
| `runner.go` | Common contract, disabled runner, root normalization, bounded preflight output, cross-platform enforcement probe |
| `runner_darwin.go` | Parameterized Seatbelt policy and launcher |
| `runner_linux.go` | Trusted Bubblewrap launcher and command arguments |
| `mounts_linux.go` | Mountinfo parsing and ordered writable/read-only overlays |
| `runner_other.go` | Fail-closed unsupported-platform backend |
| `testcontainers_integration_test.go` | Opt-in native Linux/Bubblewrap runtime validation in Debian, gated by `COAGENT_TESTCONTAINERS_INTEGRATION=1` |
| `integration_linux_test.go` | Opt-in direct-on-host Bubblewrap validation (no container), gated by `COAGENT_BWRAP_MOUNT_INTEGRATION=1`; skips if `bwrap` isn't on `PATH` |

### Platform policies

**macOS / Seatbelt**:

- `/usr/bin/sandbox-exec` receives an allow-default profile with a filtered `file-write*` denial.
- Writable paths are passed through `-D` parameters, never interpolated into policy source.
- Allow-default is deliberate: host toolchains depend on Mach, IPC, sysctl, certificate, and network services unrelated to filesystem writes.

**Linux / Bubblewrap**:

- The host root is recursively read-only (`--ro-bind / /`); writable roots are bind-mounted back at the same paths. A synthetic `/dev` exposes required devices without host `/dev/shm` writes.
- Nested host mount points below a writable root are discovered from `/proc/self/mountinfo` and re-applied read-only unless that exact mount is explicitly writable. Mount operations are broad-to-narrow so a deeper explicit root can reopen a protected parent.
- The launcher is canonical, root-owned, executable, not group/world-writable, and outside every writable root. `--unshare-user` plus `--cap-drop ALL` removes capabilities.
- No container image, network namespace, PID namespace, `setsid`, or `--chdir` is used. The existing Bash process-group cancellation model therefore remains valid.

Validation is two-layered, both fail-closed:

- **Enforcement probe (`Probe`, once per process)**: writes into a private temp allow-root, which must succeed, then writes outside it, which must fail and leave no file. A backend that launches but confines nothing — a stub `bwrap`, an inverted policy — is rejected here. It spawns sandboxed processes, so it is memoized: `main` runs it at startup when the sandbox is enabled, and a broken backend fails the daemon instead of the first session.
- **Launcher preflight (`New`, per runner)**: runs `:` under the actual roots of that session, so backend setup must succeed before Bash registration.

Runtime launcher failures after preflight remain ordinary Bash/file-mutation errors; execution never falls back to an unconfined path.

Native Linux behavior is covered by the opt-in `TestLinuxSandboxInTestcontainer` integration. It cross-compiles the current checkout's `bashsandbox` and `builtin` test binaries, runs the ordinary policy tests as an unprivileged user in Debian, and separately exercises the root-only nested-mount protection. Run it with `COAGENT_TESTCONTAINERS_INTEGRATION=1 mise exec -- go test -count=1 -v -timeout 20m ./internal/bashsandbox -run '^TestLinuxSandboxInTestcontainer$'`; the default suite skips it when no container runtime is expected.

### Configuration

```yaml
tools:
  bash:
    sandbox:
      enabled: true
      writable_paths:
        - ~/.npm
```

The existing Bash-scoped config key applies to Bash descendants and direct mutations from `write`, `edit`, and `apply_patch`. Every session reads it from the daemon process config.

### Deliberate limitations

- All read paths are unrestricted, including Bash and the built-in `read`/`ls`/`glob`/`grep` tools. Sensitive files remain visible to the agent.
- Network, Unix-domain sockets, and MCP can disclose readable data or cause effects outside filesystem mediation.
- System temp and the per-user cache directory are broad writable compatibility roots. Tool-specific caches must be listed when they live elsewhere.
- A pre-existing hard-link inside an allowed root aliases the same inode outside it; path policy cannot undo that alias.
- Seatbelt does not revoke already-open descriptors. This command path inherits only standard IO pipes.
- Dedicated `write`/`edit`/`apply_patch` mutations run through a fixed Bash helper under the same runner. External MCP/LSP processes remain outside the boundary and may write according to their own permissions.

<a id="pkg-builtin"></a>

## `internal/tool/builtin`

_Built-in tool implementations (bash, read, write, edit, glob, grep, ls, lsp, memory, batch, ...) and `BuildStack` — the registry+LSP+MCP wiring `session` builds its tool stack from._


### Purpose

This package implements every tool that doesn't need daemon- or schedule-level state: filesystem (read/write/edit/apply_patch/glob/grep/ls), process (bash), LSP, HTTP (webfetch), skills, todo, memory (recall/save/delete), and parallel dispatch (batch). It also owns `BuildStack`, which assembles a tool registry + LSP manager + MCP service for a working directory. It is the one package allowed to reach across `bashsandbox`/`loader`/`lsp`/`mcp`/`memory`/`todo` to do this (the "capability hub" in `.go-arch-lint.yml`).

### Key types

| Name | Exported | Role | Owns |
|---|---|---|---|
| `StackConfig` | yes (struct) | Input to `BuildStack`: `WorkDir`, `Pool` (`mcp.Pool`, may be nil), `Unified` (`*config.UnifiedConfig`, for MCP and Bash policy), `Loader` (`loader.Service`), `Todo` (`todo.Service`) | N/A (plain data) |
| `Stack` | yes (struct) | Output of `BuildStack`: the assembled tool registry plus its LSP/MCP lifecycle | `Registry` (exported `tool.Registry`), `lspMgr` (unexported `lsp.Manager`), `mcpSvc` (unexported `mcp.Service`, nil if no MCP servers configured) |
| Individual tool structs | mostly no (`bashTool`, `readTool`, `writeTool`, `editTool`, etc.; `LsTool` and `BatchTool` remain package-boundary construction seams) | Each implements `tool.Tool` | Immutable config + injected service references |

### File map

| File | What it contains |
|---|---|
| `stack.go` | `StackConfig`, `Stack`, `BuildStack(ctx, cfg) (*Stack, error)`, `(*Stack) Close() error`, unexported `registerCoreTools` (fixed tool-ID registration order -- feeds the prompt-cache key: `read`, `write`, `edit`, `apply_patch`, `ls`, `glob`, `grep`, `bash`, `webfetch`, `skill`, `todoread`, `todowrite`, `batch`, `lsp`) |
| `bash.go` | private `bashTool` — shell command execution through an injected `bashsandbox.Runner`, with timeout and process-group killing |
| `file_mutator.go` | private `fileMutator` plus direct and native-runner implementations shared by `write`, `edit`, and `apply_patch` |
| `process.go` | Process-group cancellation shared by Bash and sandboxed file mutations |
| `read.go` | private `readTool` — file reading with line numbers and binary detection |
| `write.go` | private `writeTool` — file creation/overwrite with LSP diagnostics |
| `edit.go` | private `editTool` — exact string-matching edits (`old_string`/`new_string` unique replacement and `replace_all` global replacement), with LSP diagnostics |
| `edit_apply.go` | Edit application logic: `executeStrReplace()`, `executeReplaceAll()`, `formatContextPreview()`, `tieredReplaceAllPreview()`, `ReplaceAllParams`, `editRange` |
| `glob.go` | private `globTool` — file pattern matching via doublestar, sorted by mtime |
| `grep.go` | private `grepTool` — regex search across files with context lines |
| `ls.go` | `LsTool` — directory listing with sizes, dirs-first sort |
| `lsp_tool.go` | private `lspTool` — LSP operations (definition, references, hover, symbols, call hierarchy, implementation) |
| `apply_patch.go` | private `applyPatchTool` — unified diff patch application |
| `batch.go` | `BatchTool` — parallel execution of up to 25 tool calls via the registry; implements `tool.RegistryBound` so a filtered/cloned registry re-targets it at its own tool set |
| `todoread.go` | private `todoReadTool` — reads todo list from `todo.Service` |
| `todowrite.go` | private `todoWriteTool` — replaces entire todo list via `todo.Service` |
| `memory_save.go` | `NewMemorySaveTool(store memory.CuratedStore, projectID int64, onChanged func(context.Context)) tool.Tool` — saves curated memory, triggers `onChanged` |
| `memory_delete.go` | `NewMemoryDeleteTool(store memory.CuratedStore, projectID int64, onChanged func(context.Context)) tool.Tool` — deletes a curated memory owned by `projectID`, triggers `onChanged` |
| `skill.go` | private `skillTool` — model-visible discovery/execution, canonical `<skill>` rendering, `$ARGUMENTS` substitution, and transcript-envelope extraction helpers |
| `webfetch.go` | private `webFetchTool` — HTTP GET with HTML-to-text conversion; `normalizeFetchURL` restricts the scheme to http/https and defaults a bare host to HTTPS without rewriting an explicit `http://` |
| `webfetch_dialer.go` | Destination policy for `webFetchTool`: address normalization (zone strip, NAT64 unwrap, unmap), the link-local/metadata predicate, the `net.Dialer.ControlContext` hook that refuses those addresses after DNS resolution and before `connect`, and the cloned transport with `Proxy` disabled |
| `utils.go` | `resolvePath()` — path resolution helper shared by filesystem tools |
| `result_keys.go` | `tool.Result.Metadata` key constants shared across tools (`metaKeyPath`, `metaKeyCount`, `metaKeyTruncated`, `metaKeyExitCode`, `metaKeyTimedOut`) |

Note: `memory_save.go`/`memory_delete.go` take `memory.CuratedStore` **directly** -- the former `tool.CuratedMemoryStore` DI interface is gone. `task`, `get_subagent_result`, `send_to_subagent`, `schedule`, `sleep`, and `compact_context` are **not** in this package -- they live in `daemon` and `session`/`schedule` respectively, since they need state this package intentionally has no access to.

### State ownership

| State | Location | Source of truth | Synchronization | Failure behavior |
|---|---|---|---|---|
| Tool map | (delegates to `tool.Registry`, built fresh by `BuildStack`) | Registry instance | `tool` package's `svc.mu` | Registry methods return nil/false for missing tools |
| Filesystem (all file tools) | OS | The filesystem itself | None — tools trust single-writer semantics from the agent loop | OS errors propagated to caller |
| LSP diagnostics cache | `lsp.Manager` (owned by `Stack.lspMgr`) | LSP servers | Not owned by this package's tools directly | Silently skipped if lspMgr is nil |
| MCP servers | `mcp.Service` (owned by `Stack.mcpSvc`) | MCP subprocess or pool | N/A | `BuildStack` logs a warning and degrades to builtin-only tools on acquisition failure -- never fails session creation |
| Bash runner | `bashTool.runner` | `bashsandbox.New` | Immutable | Enabled backend/config/preflight failure aborts `BuildStack`; no unsandboxed fallback |
| File mutator | `writeTool`/`editTool`/`applyPatchTool` | `newFileMutator` from the same runner and enablement | Immutable | Enabled helper failure propagates; no direct-write fallback |
| WebFetch HTTP client | `webFetchTool.client` | `newRestrictedTransport` at construction | Immutable; `http.Client` is goroutine-safe | Blocked destination fails the dial; no fallback path |

### Concurrency model

**`BatchTool`**: Spawns one goroutine per call via `sync.WaitGroup`. All goroutines share the parent context. Results are written to a pre-allocated slice indexed by position — no mutex needed because each goroutine writes to a distinct index.

**`bashTool`**: Requests `*exec.Cmd` from the injected `bashsandbox.Runner`, then creates an OS process group (`Setpgid: true`) and kills the entire group on context cancellation via `cmd.Cancel`. Uses `WaitDelay` of 5 seconds as a backstop.

**Sandboxed file mutations**: Stream content through stdin to a fixed helper command with paths passed only as positional arguments. They use the same process-group cancellation and `WaitDelay` as `bashTool`; helper diagnostics are bounded to 8 KiB.

**No goroutines are long-lived in this package.** All concurrency is request-scoped (within a single `Execute` call).

### Data flow

#### `BuildStack` (called by `session.factory.build`)
```
BuildStack(ctx, StackConfig{WorkDir, Pool, Unified, Loader, Todo})
  -> bashsandbox.New({Enabled, WorkDir, WritablePaths})
       -> disabled: plain Bash runner, no backend probe
       -> enabled: normalize roots + native backend preflight
       -> on error: abort before LSP/MCP resources exist
  -> newFileMutator(Enabled, bashRunner)
       -> disabled: direct os.MkdirAll/os.WriteFile
       -> enabled: fixed helper through bashRunner, no fallback
  -> tool.NewRegistry()
  -> lsp.NewManager(provider)
  -> registerCoreTools(registry, workDir, loader, todo, lspMgr, bashRunner, fileMutator) // fixed order
  -> mcp.AcquireForWorkDir(ctx, pool, unified, workDir)
       -> pool-or-direct acquisition; nil,nil if no MCP servers configured
       -> on error: log warning, degrade to builtin-only (Stack still returned, mcpSvc=nil)
  -> if mcpSvc != nil: mcpSvc.RegisterTools(registry)
  <- &Stack{Registry, lspMgr, mcpSvc}
```

#### File edit path
```
Agent loop
  -> Registry.Execute("edit", params)
    -> editTool.Execute()
      -> os.ReadFile(path)
      -> executeStrReplace() or executeReplaceAll()  [edit_apply.go]
      -> fileMutator.WriteFile(ctx, path, newContent, false)
           -> disabled: direct os.WriteFile
           -> enabled: fixed helper through bashsandbox.Runner
      -> lspMgr.TouchFile() + sleep(150ms) + lspMgr.GetAllDiagnostics()
      -> Result{Output: preview + diagnostics}
```

### Lifecycle

**`Stack`**: Created via `BuildStack`. `Close()` closes `lspMgr` (if non-nil) then stops `mcpSvc` (if non-nil) -- idempotent-by-construction since it's called exactly once from `session.Close()`. Native write-policy setup is the only fatal `BuildStack` setup step and runs before resource-owning components, so its failure has nothing to tear down.

**Individual tools**: All tools are created via `New*Tool()` constructors (or `registerCoreTools`'s unexported equivalents). They hold immutable configuration (workDir, injected services). No `Start()`/`Stop()` lifecycle — they exist for the duration of the session.

### Tensions

1. **`Description()` determinism is convention, not enforced.** `skillTool.Description()` dynamically lists `ListModelInvocableSkills()` in sorted order and caps each description at 1,536 Unicode code points.

2. **`writeTool` and `editTool` block on `time.Sleep(150ms)` for LSP diagnostics.** This hard-coded delay exists on every write/edit operation, even when no LSP server is configured (in that case, `lspMgr` methods return fast). The sleep is unconditional once `lspMgr != nil`.

3. **Curated memory is the only memory backend.** `memory_save.go`/`memory_delete.go` operate via `memory.CuratedStore` (explicit user-saved memories rendered into the system prompt). There is no automatic/semantic recall tool — the earlier extraction subsystem was removed.

4. **`BuildStack`'s MCP degrade-on-failure is silent to the model.** If MCP acquisition fails, the session gets a builtin-only registry with just a warning log -- the LLM has no signal that MCP tools are missing versus simply unconfigured.

### Ordering constraints

1. **Read before edit**: The agent should read a file via `readTool` before editing it with `editTool`. `editTool` requires an exact `old_string` match.

2. **`registerCoreTools` before MCP registration in `BuildStack`**: core tools are registered first, then MCP tools are added on top -- an MCP server exposing a tool with the same name as a core tool silently wins (registry `Register` overwrites).

3. **Bash runner before LSP manager creation**: enabled sandbox validation and preflight can fail. It must run before `lsp.NewManager(provider)` so a rejected backend cannot leak resources.

### Package anti-patterns

1. **Don't nest `BatchTool` calls or batch skills**: `BatchTool` rejects both `batch` and `skill`; canonical skill invocations must remain direct transcript entries so compaction can recover them unambiguously.

2. **Don't call `BuildStack` twice for the same session/context and keep both `Stack`s alive.** Each holds its own LSP manager and MCP acquisition; leaking one leaks a process or a pool refcount.

3. **Don't hold a `tool.Registry` in a tool without implementing `tool.RegistryBound`.** `BatchTool` resolves and executes its callees through its registry; a copy left bound to the unfiltered stack executes exactly what the session's agent-type filter removed.

### Contracts

1. **Filesystem tools resolve paths through `resolvePath()`**: All filesystem tools (`readTool`, `writeTool`, `editTool`, `globTool`, `grepTool`, `LsTool`, `applyPatchTool`) use `resolvePath(workDir, userPath)` to normalize paths. This function expands `~`, resolves relative paths against `workDir`, and cleans absolute paths.

2. **LSP diagnostics are best-effort**: `writeTool` and `editTool` append LSP diagnostics to their output when `lspMgr != nil`. If the LSP server is slow, unreachable, or returns no diagnostics within the 150ms window, the tool still succeeds — diagnostics are informational, never blocking.

3. **editTool requires unique match**: The `old_string`/`new_string` mode in `editTool` requires that `old_string` appears exactly once in the file. Zero matches or multiple matches both cause the edit to be rejected with an error. The `replace_all` mode replaces all occurrences.

4. **`Stack.Close()` degrades gracefully**: it always returns `nil` -- LSP/MCP teardown errors are the individual `Close`/`Stop` implementations' problem, not surfaced here.

5. **Skill visibility has independent axes**: `skillTool.Description` and `skillTool.Execute` use model visibility only (`disable-model-invocation`); direct `/skill` handling lives in `session` and uses user visibility only (`user-invocable`).

6. **WebFetch refuses link-local and metadata destinations**: `webFetchTool`'s transport installs a `net.Dialer.ControlContext` hook that runs after DNS resolution and before `connect`, so the original URL, every redirect hop and any DNS rebinding are checked against the same resolved address that is dialed. Blocked: IPv4/IPv6 link-local, the IPv6 metadata endpoint, and NAT64-wrapped forms of those. Loopback, RFC1918, CGNAT and ULA stay reachable by design -- the agent must be able to reach services it is developing. `Transport.Proxy` is `nil`, so `HTTP_PROXY`/`HTTPS_PROXY` are ignored; a proxy would connect on the daemon's behalf and make the check decorative. This is a targeted mitigation, not an SSRF boundary -- Bash egress is unrestricted and reaches the same addresses.

7. **Canonical skill envelope**: `RenderSkill` emits `<skill>`, escaped `<name>`/optional `<description>`, `---`, body, and `</skill>`. `$ARGUMENTS` is replaced everywhere; otherwise non-empty arguments append as `ARGUMENTS: ...`. Extraction must preserve body text literally, including marker-like examples.

8. **File tools reject non-regular paths**: `read`/`grep`/`edit`/`write`/`apply_patch` stat the target and refuse a FIFO/device/socket before `os.Open`/`os.ReadFile`/`os.WriteFile` — opening a writer-less FIFO blocks in-kernel, uncancelable by ctx, so even session cancellation can't recover. An explicit non-regular path errors; one discovered via grep's glob is skipped. The mutation tools share one helper (`rejectNonRegular` in `utils.go`); the `lsp` tool applies the same stat gate in `ensureFileOpen`/`TouchFile`.


<a id="pkg-mcp"></a>

## `internal/mcp`

_MCP server connections, connection pooling, eviction, tool discovery, `AcquireForWorkDir`._


### Purpose

Manages MCP (Model Context Protocol) server *connections* and exposes external tools to the agent. Handles two usage modes: **direct** (session starts and owns MCP servers) and **pooled** (daemon-level connection pool with TTL-based lifecycle, shared across sessions). Wraps MCP tools as coagent `tool.Tool` implementations for seamless integration with the tool registry. `AcquireForWorkDir` is the single entry point `tool/builtin.BuildStack` (and therefore `session`) calls to get either mode without knowing which one it is.

This package does not decide **which** servers exist — it is handed already-resolved `ServerConfig` definitions with `${VAR}` env references already expanded. That set comes from `internal/mcpstore` via `session.resolveMCPServers` ([ADR-0004](adr/0004-mcp-registry-in-sqlite.md)).

### Key types

| Name | Type | Role |
|------|------|------|
| `Service` | interface | Unified API for starting/stopping MCP servers and registering their tools |
| `svc` | struct | Direct-mode `Service` implementation, owns clients and their lifecycle |
| `Pool` | interface | Daemon-level connection pool with refcount-based lifecycle and TTL reaping |
| `pool` | struct | `Pool` implementation with background reaper goroutine |
| `poolView` | struct | `Service` adapter over pool-acquired clients; `Stop()` releases refs instead of closing |
| `Client` | struct | Wraps a single MCP server subprocess (stdio transport), holds discovered tools |
| `mcpTool` | private struct | Adapts an MCP tool to the `tool.Tool` interface |
| `Config` | struct | Top-level MCP config: map of server name to `ServerConfig` |
| `ServerConfig` | struct | Single server config: command, args, env, workdir, enabled/disabled |
| `ServerStats` | struct | Startup statistics (total, started, failed, skipped) |
| `poolEntry` | struct | Internal pool bookkeeping: client, server name, refcount, `evicted` flag, lastUsed timestamp |
| `failedEntry` | struct | Negative-cache entry: when a server start failed + the workdir env fingerprint then, for the retry cooldown |

Compile-time checks: `var _ Service = (*svc)(nil)`, `var _ Pool = (*pool)(nil)`, `var _ Service = (*poolView)(nil)`.

### File map

- `mcp.go` -- `Service` interface, `svc` implementation (direct mode), `ServerStats`. Concurrent server startup, tool registration, client lifecycle
- `acquire.go` -- `AcquireForWorkDir(ctx, pool Pool, servers map[string]ServerConfig, workDir string, provider shellenv.Provider) (Service, error)`: `stampWorkDir` binds the caller's definitions to this session's workdir (part of the pool identity hash) and drops disabled ones, then pool-acquires (if `pool != nil`) or starts directly (`startDirect`, which threads `provider`); returns `(nil, nil)` if no servers are configured; direct-mode start failures are logged, not fatal (`Service` is still returned)
- `pool.go` -- `Pool` interface, `pool` implementation. `NewPool(provider shellenv.Provider) Pool` public constructor (folds `provider` into the client factory so every pooled spawn routes through per-cwd activation, and wires `provider.Fingerprint` for the negative cache; `main.go` calls `pool.Stop()` explicitly as a named stop closure); `newPoolFP(ttl, fpFn, factory)`/`newPool(factory)` internal constructors for tests. Refcount-based Acquire/Release, TTL reaper goroutine. A **failed server start is skipped, not fatal** (logged `warn`, the acquire still returns whatever came up) so one broken MCP server never blocks the session; a **negative cache** (`failed` map) then gates its retry with a short cooldown, invalidated early when the workdir env fingerprint changes. Rollback survives only for the "pool stopped mid-acquire" path. `Stop`/`reap`/`Evict`/`Release` collect the clients to close under `pool.mu`, then `Close()` them **outside** the lock, so a bounded-but-nonzero teardown can't serialize every session's Acquire/Release. `Evict(serverName)` retires every entry for a name when its registry row is removed or disabled: refcount 0 closes immediately, refcount > 0 sets `evicted` so the entry dies on its last `Release` rather than under an active call
- `poolview.go` -- `poolView` adapter: wraps pool-acquired clients as a `Service`. `Stop()` calls `pool.Release()` instead of closing clients
- `client.go` -- `Client` struct. `NewClient(ctx, name, cfg, provider)` spawns the MCP server subprocess via stdio transport — its `WithCommandFunc` routes the spawn through `provider.WrapExec(ctx, cfg.WorkDir, [command,args…], envList)` (server env survives as non-colliding extraEnv) — then performs the handshake (Initialize) and tool discovery (ListTools) **under a bounded `initTimeout` (30s)** so a server that spawns but never speaks the protocol can't wedge session startup, and **owns the subprocess lifetime** via a detached cancel func (`cancelRun`) so `Close()` force-kills a live-but-mute child instead of blocking on `cmd.Wait()`. Proxies CallTool requests **under a bounded `callTimeout` (5 min)** so a live-but-unresponsive tool call can't hang the session loop
- `tool.go` -- private `mcpTool` struct. Adapts MCP tools to `tool.Tool` interface with ID format `mcp__{server}__{tool}`
- `config.go` -- `Config`, `ServerConfig`. Deterministic SHA-256 hashing for pool dedup, `IsEnabled()` logic

### State ownership

| State | Location | Source of truth | Synchronization |
|-------|----------|-----------------|-----------------|
| Active clients (direct mode) | `svc.clients` map | In-memory | `svc.mu` (RWMutex) |
| Startup stats (direct mode) | `svc.stats` | In-memory | `svc.mu` (RWMutex) |
| Pooled client entries | `pool.entries` map | In-memory | `pool.mu` (Mutex) |
| Negative cache (failed starts) | `pool.failed` map | In-memory | `pool.mu` (Mutex) |
| Deferred-close marks | `poolEntry.evicted` | In-memory | `pool.mu` (Mutex) |
| Pool stopped flag | `pool.stopped` | In-memory | `pool.mu` (Mutex) |
| Reaper goroutine | `pool.done` channel | -- | Channel close |
| Pool view release state | `poolView.stopOnce` | -- | sync.Once |
| MCP subprocess | OS process (via mcp-go) | OS | Context cancellation |

No persistent state. MCP connections are ephemeral -- they are re-established on session creation or pool acquire.

### Concurrency model

#### Mutexes

| Mutex | Scope | Protects | IO under lock? |
|-------|-------|----------|----------------|
| `svc.mu` | `svc` (direct mode) | `clients` map, `stats` | No |
| `pool.mu` | `pool` | `entries` map, `stopped` flag | **Temporarily released** during client creation in `Acquire` |
| Local `mu` in `svc.Start` | Single `Start` call | `started`/`failed`/`skipped` counters | No |

#### Goroutines

| Goroutine | Spawned by | Shutdown |
|-----------|-----------|----------|
| Reaper | `pool.startReaper()` (via `reaperOnce`) | `close(p.done)` in `Stop()` |
| Per-server startup | `svc.Start()` | `sync.WaitGroup` -- caller blocks until all complete |
| Client creation timeout | `svc.startServer()` | Context timeout (2min) + select |

#### Pool Acquire unlock-relock pattern

`pool.Acquire` must create MCP clients (subprocess spawn + handshake), which blocks. It follows the unlock-relock pattern from HLA rule 4:

1. Lock, check if entry exists (cache hit)
2. If miss: **unlock**, create client via factory (IO)
3. **Re-lock**, double-check entry (another goroutine may have created it)
4. If race detected: close the duplicate client, use existing entry
5. If pool stopped during unlock: close client, rollback all acquired entries

### Data flow

#### Direct mode (standalone Service)
```
session.factory -> mcp.New(workDir, provider) -> svc
  -> svc.Start(ctx, config)
    -> for each enabled server: goroutine -> startServer()
      -> NewClient(ctx, name, cfg, provider)   // WithCommandFunc routes the spawn through provider.WrapExec
        -> StdioMCPClient (subprocess) -> Initialize -> ListTools
      -> store in svc.clients
  -> svc.RegisterTools(registry)
    -> for each client, for each tool: newMCPTool -> registry.Register
  -> session runs, calls mcp__server__tool -> mcpTool.Execute -> client.CallTool
  -> svc.Stop() -> close all clients
```

#### Pooled mode (daemon-level Pool)
```
cmd/coagent -> mcp.NewPool(provider) -> pool (starts reaper immediately; main.go registers pool.Stop
                                       as its own named stop closure; provider folds into the pool factory)
  -> builtin.BuildStack -> mcp.AcquireForWorkDir(ctx, pool, uc, workDir, provider)
    -> pool.Acquire(ctx, configs) -> create or reuse clients, increment refcounts
    -> newPoolView(pool, clients, hashes) -> poolView
  -> poolView.RegisterTools(registry) (same as direct)
  -> session runs, uses tools
  -> poolView.Stop() -> pool.Release(hashes) -> decrement refcounts
  -> reaper (every 1min) -> reap entries where refcount=0 && TTL expired
  -> pool.Stop() at process shutdown (main.go's named stop closure) -> close all remaining clients
```

#### Tool call path
```
LLM -> tool call "mcp__tavily__tavily_search" -> tool.Registry.Get()
  -> mcpTool.Execute(ctx, params)
    -> json.Unmarshal params -> client.CallTool(ctx, name, args)
      -> mcp-go CallToolRequest -> subprocess stdin/stdout
    <- concatenate text content -> tool.Result{Title, Output, Metadata}
```

### Lifecycle

#### Direct mode (`svc`)
1. `New(workDir)` -- creates empty service
2. `Start(ctx, config)` -- starts all enabled servers concurrently (2min timeout per server), populates `clients` and `stats`
3. `RegisterTools(registry)` -- wraps each discovered tool as private `mcpTool`, registers in tool registry
4. `GetClient(name)` -- lookup by server name (used for session-level MCP instructions)
5. `Stop()` -- closes all clients, clears map

#### Pooled mode (`pool` + `poolView`)
1. `NewPool()` -- creates pool with `defaultTTL` (30min, package-level const), starts reaper goroutine immediately. Internally calls `newPool(defaultTTL, NewClient)`. No lifecycle parameter -- `main.go` owns calling `Stop()`.
2. `pool.Acquire(ctx, configs)` -- returns clients (creating new or incrementing refcount on existing), returns config hashes for later Release
3. `newPoolView(pool, clients, hashes)` -- wraps acquired clients as a `Service`
4. `poolView.RegisterTools(registry)` -- same as direct mode
5. `poolView.Stop()` -- calls `pool.Release(hashes)`, idempotent via `sync.Once`
6. Pool reaper (background, every 1min) -- closes entries with `refcount=0` and `lastUsed` beyond TTL
7. `pool.Stop()` -- closes all entries, stops reaper. Called explicitly by `main.go`'s named stop closure (`onStop("mcp.pool", pool.Stop)`), registered right after construction so it runs **last** in the reverse-order shutdown.

#### Constructor split: `NewPool` vs `newPool`

`NewPool()` is the public constructor for production use. It hardcodes `defaultTTL` and `NewClient` as the factory. `newPool(ttl, factory)` is the internal constructor used by tests -- it accepts custom TTL and a mock factory. Tests call `newPool` directly and `defer p.Stop()` manually.

### Tensions

- **Pool unlock during client creation**: `Acquire` must release the mutex while spawning subprocesses (HLA rule 4). This creates a TOCTOU window where another goroutine can create the same entry. Handled by double-checking after re-lock and closing the duplicate. The alternative (holding the lock) would serialize all MCP server startups across all sessions.

- **Partial failure in Acquire**: If creating the Nth server fails, previously acquired entries must be rolled back (refcounts decremented). `rollbackLocked` handles this, but the already-created clients remain in the pool with refcount=0 (they weren't closed -- they'll be reaped by TTL). This is intentional: another session might acquire them before reaper runs.

- **Direct mode vs pooled mode coexistence**: Both `svc` and `poolView` implement `Service`, but with fundamentally different ownership semantics. `svc.Stop()` closes clients (owns them). `poolView.Stop()` releases refs (borrows them). The `Service` interface doesn't expose this distinction -- callers must know which they're using. In practice, `session.factory.acquireMCP` decides: pool mode if `MCPPool` is set, direct mode otherwise.

- **Client.Close errors silently discarded**: Several paths (`_ = client.Close()`) discard close errors. MCP subprocess cleanup is best-effort -- the subprocess may have already exited, and there's no recovery action for a failed close.

### Ordering constraints

- **Pool reaper starts immediately**: `newPool` starts the reaper goroutine in the constructor. There is no separate `Start()` call. Callers must call `Stop()` to clean up (in production, `main.go` does this via its named stop closure).
- **Initialize before ListTools**: `NewClient` must complete the MCP handshake (`Initialize`) before calling `ListTools`. This is enforced sequentially in the constructor.
- **Acquire before RegisterTools**: Tools can only be registered after clients are acquired (direct: after `Start`, pooled: after `Acquire`/`newPoolView`).
- **Release after all tool calls complete**: `poolView.Stop()` (which calls `Release`) must not happen while tool calls are in flight. The session ensures this by stopping tool execution before calling `Close()`.
- **pool.Stop() after all sessions closed**: `main.go` registers the pool's stop closure first (right after construction), so the reverse-order shutdown runs it **last** -- after the server, managers, schedule executor, daemon, and DB have all stopped. This guarantees every session (and its poolView) is gone before the pool tears down its clients.

### Anti-patterns

- **Don't call `svc.Start` twice**: The second call would create duplicate clients alongside existing ones. There's no guard against this -- the caller (session factory) must ensure single invocation.
- **Don't mix pool and direct clients**: A `poolView` must only be created from `pool.Acquire` results. Manually constructing a `poolView` with non-pool clients would cause `Release` to decrement refcounts for entries that don't exist, which is a no-op but indicates a logic error.
- **Don't hold references to pool clients after Release**: After `poolView.Stop()`, the underlying clients may be reaped at any time. The `poolView` still holds client pointers, but using them after Stop is undefined.
- **Don't create MCP clients without timeout**: `NewClient` can block indefinitely on subprocess startup or handshake. Always use a context with timeout (direct mode uses `defaultMCPStartTimeout` = 2min).
- **Don't construct a `Pool` in production without registering its `Stop()` at the composition root**: `NewPool()` starts the reaper goroutine immediately but registers no shutdown hook itself -- `main.go` must register `pool.Stop` as a named stop closure right after construction, or the reaper goroutine and open clients leak on shutdown.

### Contracts

- **Config hash is identity for pooling**: Two `ServerConfig` values with the same `Hash()` are considered the same server. Hash includes command, args (order-preserving), env (sorted), and workdir. It excludes `Disabled`/`Enabled` flags -- two configs differing only in enabled state share a pool entry.
- **Tool ID format**: `mcp__{serverName}__{toolName}`. This is a stable contract used by the LLM to reference tools. Changing the format would break existing conversation histories.
- **Refcount invariant**: `poolEntry.refcount >= 0` always. `Release` clamps to 0 on underflow. Entries with `refcount > 0` are never reaped.
- **Reaper is best-effort**: The reaper runs every 1 minute. An idle client may live up to `TTL + reaperInterval` before cleanup. This is acceptable -- MCP subprocesses are lightweight.
- **poolView.Stop is idempotent**: Safe to call multiple times via `sync.Once`. Critical because session cleanup paths may call Stop from multiple goroutines.
- **svc.Stop closes all clients unconditionally**: Unlike poolView, direct-mode Stop does not check refcounts. The caller (session) must ensure no tool calls are in flight.
- **Acquire is all-or-nothing**: If any server in the config map fails to start, all previously acquired entries are rolled back and an error is returned. No partial results.
- **defaultTTL is a package-level constant** (30min). Not configurable at runtime. Change requires code modification and recompile. `reaperInterval` (1min) is similarly a package-level constant.

### Test patterns

Tests use `newPool(ttl, factory)` directly with mock factories and `nopMCPClient` stubs. They assert on internal state (`pool.entries`, `poolEntry.refcount`) via type assertion `p.(*pool)` under lock. Table-driven tests cover hash determinism, dedup, refcount lifecycle, rollback on factory error, stop idempotency, and acquire-after-stop. White-box testing is acceptable here because pool internals are the contract -- external behavior alone can't verify refcount correctness.


<a id="pkg-mcpstore"></a>

## `internal/mcpstore`

_MCP server registry (`mcp_servers` table): global and project-scoped server definitions._


### Purpose

The MCP **registry** — which servers exist and for whom — as distinct from the MCP **pool**, which owns their running connections. A leaf over `*sql.DB`, shaped like `memory.CuratedStore`: plain CRUD, no caching, no lifecycle ([ADR-0004](adr/0004-mcp-registry-in-sqlite.md)).

Scoping is one nullable column: `project_id IS NULL` is a global row, non-NULL belongs to one project. A session's server set is the enabled globals merged with the enabled rows of its project, project winning on a name collision.

It stores definitions only. `${VAR}` env references are kept **literally** — resolution against the in-memory secrets map happens at acquire time in `session.resolveMCPServers`, so no credential is ever written to the database by this package.

### Key types

| Name | Type | Role |
|------|------|------|
| `Store` | interface | `Add` / `Remove` / `SetEnabled` (all `*int64` project id: nil = global), `ListForProject` (the merged, enabled set a session gets), `ListAll` (every row, disabled included, split by scope) |
| `store` | struct | Implementation over `*sql.DB` |
| `ServerDef` | struct | One row: `Name`, `Command`, `Args`, `Env`, `Enabled` |
| `ErrDuplicate` / `ErrNotFound` | sentinel errors | A name already in the target scope / absent from the scope addressed |

### File map

- `mcpstore.go` — `Store`, `store`, `ServerDef`, sentinels, `Add`/`Remove`/`SetEnabled`/`ListForProject`/`ListAll`
- `query.go` — the read side and its helpers: `exists`, `requireAffected`/`otherScopeHint` (the not-found message that names the other scope), `query`/scan, args/env JSON (de)serialization, scope naming

### State ownership

All state is in SQLite (`mcp_servers`, migration `00016`). No in-memory cache — the per-iteration stack rebuild reads current rows every time, which is the entire propagation mechanism. `*sql.DB` is goroutine-safe; no package-level mutable state.

Uniqueness is two **partial** indexes, not one plain `UNIQUE`: SQLite treats NULLs as distinct, so `UNIQUE(project_id, name)` would happily accept two identical global rows.

### Ordering constraints

**A disabled project row shadows the global, it does not fall back to it.** `ListForProject` records every project name as taken *before* filtering on `Enabled`. Disabling the project's own row means "not here", which is the only reading that lets a project switch off an inherited global.

**Duplicate detection is per scope.** Adding a project row whose name shadows a global one is the override working as intended; only a clash within the same scope is `ErrDuplicate`.

### Package anti-patterns

**Don't resolve secrets here.** Env values go in and come out verbatim. The moment this package expands a reference, resolved credentials start landing in the database.

**Don't add health or liveness to the listing.** "Status" here means the `enabled` flag. Whether a subprocess is currently up is the pool's business, and answering it would couple the registry to pool internals.

### Contracts

- **Name collision resolves project-over-global**, including when the project row is disabled.
- **`ListForProject` returns only enabled rows**; `ListAll` returns everything and is what the `mcp_list` tool renders.
- **Not-found errors name the scope searched** and, when the name exists in the other scope, say so — "wrong scope" must not read as "never existed".

<a id="pkg-memory"></a>

## `internal/memory`

_Curated per-project memory: plain SQLite CRUD over the `memories` table._


### Purpose

Stores explicit, user-controlled memory entries for agent sessions. Short text
facts saved and deleted via the `memory_save` / `memory_delete` tools and
rendered into the system prompt. No embeddings, no chunking, no search — plain
CRUD scoped by project. (An earlier automatic extraction/embedding subsystem was
removed; nothing read it, and the OpenAI-key dependency did not belong in a
self-hosted core.)

### Key types

| Name | Type | Role |
|------|------|------|
| `CuratedStore` | interface | CRUD for curated memories, every method project-scoped: `SaveMemory`, `DeleteMemory`, `ListMemories`, `CountMemories`, `ListMemoryTexts` |
| `curatedStore` | struct | `CuratedStore` implementation |
| `MemoryEntry` | struct | Curated memory for display (ID + text) |
| `CuratedMemory` | struct | Full curated memory row (ID, project ID, text, timestamp) |

Compile-time check: `var _ CuratedStore = (*curatedStore)(nil)`.

### File map

- `store.go` — `CuratedStore` interface, `curatedStore` implementation, `NewCuratedStore`, all CRUD methods
- `types.go` — `MemoryEntry` data type

No `migrate.go` — all schema migrations live in `internal/migrate/`.

### Constructors

#### `NewCuratedStore(db *sql.DB) CuratedStore`
Creates the curated memory store over the shared daemon `*sql.DB` handle. No
bootstrap side effects — the `memories` table is created by migrations.

### State ownership

| State | Location | Source of truth | Notes |
|-------|----------|-----------------|-------|
| Curated memories | SQLite `memories` table | DB | Plain text, project-scoped |

### Concurrency model

- **No mutexes, no goroutines.** The package is stateless beyond the `*sql.DB` handle; SQLite handles its own locking. All operations are synchronous — the caller provides the goroutine.

### Data flow

```
builtin.memorySaveTool   -> curatedStore.SaveMemory(ctx, projectID, text)     -> INSERT INTO memories
builtin.memoryDeleteTool -> curatedStore.DeleteMemory(ctx, projectID, id)     -> DELETE FROM memories
session.buildMemoriesSection / refreshMemories -> ListMemoryTexts(ctx, projectID) -> SELECT from memories
```

(`memory_save.go` / `memory_delete.go` live in `internal/tool/builtin` and hold
`memory.CuratedStore` directly, no DI interface. The prompt builder reads it via
`ListMemoryTexts` to render the `# YOUR MEMORIES` section.)

### Lifecycle

1. **CuratedStore**: created at daemon startup via `NewCuratedStore(db)`; survives the entire process.
2. **Close**: none — the store does not own the `*sql.DB` handle (the daemon owns and closes it last during shutdown).

### Anti-patterns

- **Don't hold the `*sql.DB` handle after daemon shutdown.** The daemon closes the DB; any call afterwards fails with a closed-database error.
- **Don't add search/embedding back into this package.** Curated memory is deliberately plain CRUD surfaced in the prompt; at its scale (≤50 entries/project) indexes add nothing.

### Contracts

- **`DeleteMemory` returns an error if the ID is not found.** Checks `RowsAffected()` and returns a formatted string error (not a sentinel — tech debt).
- **Project scoping.** All operations are scoped by `projectID`, including `DeleteMemory(ctx, projectID, id)`: the `DELETE` carries `AND project_id = ?`, so an id owned by another project is reported as not found and its row survives.

### Test patterns

Tests use `testify/assert` + `testify/require` over a migrated temp SQLite DB
(`store_test.go`). Table-driven where it helps. No integration tests against
external services.


<a id="pkg-registry"></a>

## `internal/registry`

_Agent-type taxonomy, built-in types, prompt templates, immutable per-session `Set`._


### Purpose

Defines the agent type taxonomy and provides configuration lookup for all agent types (built-in and custom). This is the single place that maps agent type names to their capabilities: allowed tools, system prompts, descriptions, and optional model overrides. The package also contains all prompt templates used across the system (build, general, explore, compaction).

The registry is purely declarative — it defines what agent types exist and what they can do, but does not create or execute agents. Session and daemon packages consume this configuration to wire up sessions. Since the July refactor, there is **no package-level mutable state at all**: `Set` is an immutable, per-session snapshot built once and never modified.

### Key types

| Name | Type | Role |
|------|------|------|
| `AgentType` | `string` type | Named agent configuration key (`"build"`, `"general"`, `"explore"`, `"compaction"`) |
| `AgentTypeConfig` | struct | Full configuration for an agent type: `Name`, `Description`, `Mode`, `Tools` ([]string spec), `Prompt`, `Model` (optional override), `MaxIterations` |
| `AgentTypes` | package-level `var map[AgentType]AgentTypeConfig` | The 4 built-in types (build, general, explore, compaction). Never mutated after init | -- |
| `Set` | exported struct | Immutable per-session agent-type catalog: built-ins overlaid with the session's project-local subagents | `types map[AgentType]AgentTypeConfig` |

No exported interfaces on `Set` itself — it's a plain struct with methods, safe for concurrent reads because it's never mutated after `NewSet` returns.

### File map

- `registry.go` — `AgentType`/`AgentTypeConfig` types, built-in `AgentTypes` map, private `defaultSubagentMaxIterations` const, `Set` struct, `NewSet(projectSubagents []AgentTypeConfig) *Set`, `(*Set) Get`/`Has`/`ListSubagents`/`FilterTools`, `normalizeSubagent`/`excludeTodoTools` (unexported helpers applying subagent defaults)
- `prompts.go` — All prompt constants: `BuildAgentPrompt`, `GeneralAgentPrompt`, `ExploreAgentPrompt`, `CompactionInitialPrompt`, `CompactionMergePrompt`, `PostCompactionAssistantAck`, `PostCompactionPrimer`

### State ownership

| State | Location | Source of truth | Synchronization |
|-------|----------|-----------------|-----------------|
| Built-in agent types | `AgentTypes` package-level map | Compile-time constant | Immutable after init — no synchronization needed |
| Per-session agent-type catalog | `Set.types` (held on `session.svc.agentTypes`) | Built once by `NewSet`, then read-only | None needed — never mutated after construction |

The former package-level mutable `dynamicAgents` map (and its `dynamicAgentsMu` mutex) is **gone**. Custom subagents from `.claude/agents/*.md`/`.coagent/agents/*.md` are loaded by `loader` into `[]registry.AgentTypeConfig` (via `session/setup.go`'s `subagentConfigs`), then folded into a fresh, session-scoped `*Set` by `registry.NewSet` — no cross-session global state, no accumulation, no unregistration problem.

### Concurrency model

No mutex anywhere in this package. `AgentTypes` is a read-only package-level map. `Set` is built once (`NewSet`) before any concurrent access begins and never mutated afterward — every `Get`/`Has`/`ListSubagents`/`FilterTools` call is a pure read on immutable data, safe to call from any goroutine without synchronization.

### Data flow

#### Agent type resolution (session creation)

```
session.newWithOptions(opts)
    → loadProjectContext → []registry.AgentTypeConfig (project subagents)
    → registry.NewSet(projectSubagents) → *Set  (built-ins + overlay, in one shot)
    → set.Get(agentType) → (AgentTypeConfig, ok)   // setup runs BEFORE this resolution
    ← AgentTypeConfig{Prompt, Tools, Model, MaxIterations, ...}
    → session uses config.Prompt as system prompt
    → session uses config.Model for LLM client selection
    → session.agentTypes = set   // stored for AgentTypes()/FilterTools use later (e.g. daemon's registerGated)
```

#### Custom subagent overlay (`NewSet`)

```
loader.LoadSubagents(workDir) → svc.subagents map (loader-internal)
subagentConfigs(ctx, loader, models) → ldr.ListSubagents() → []registry.AgentTypeConfig
                                        (an unresolvable `model:` is dropped + warned here)

registry.NewSet(projectSubagents):
    types := copy of AgentTypes (built-ins)
    for each projectSubagent:
        types[cfg.Name] = normalizeSubagent(cfg)   // shadows a built-in of the same name
    return &Set{types: types}
```

#### Tool filtering (subagent tool restriction, and daemon's control-plane gating)

```
session.filterRegistryForAgent(set, reg, agentConfig):
    → set.FilterTools(allToolIDs, agentType)
        → parse tool spec: "*" = include all, "-name" = exclude, "name" = include
        ← filtered tool ID list

daemon.registerGated(sess, reg, t):     // gates task/schedule/sleep/subagent-monitoring tools
    → sess.AgentTypes().FilterTools([]string{t.ID()}, sess.AgentType())
    → len(...) == 0 ? skip registration : reg.Register(t)
```

#### Tool spec DSL

The `Tools` field in `AgentTypeConfig` is a mini-DSL:
- `"*"` — include all tools from the parent's registry
- `"-toolname"` — exclude a specific tool (only meaningful with `"*"`)
- `"toolname"` — include a specific tool (explicit allowlist)

Examples from built-in types:
- `build`: `["*"]` — everything
- `general`: `["*", "-todoread", "-todowrite"]` — everything except todo tools
- `explore`: `["read", "grep", "glob", "ls", "bash"]` — explicit allowlist
- `compaction`: `[]` — no tools (LLM-only summarization)

`normalizeSubagent` applies three defaults to project-local subagents: `defaultSubagentMaxIterations` (50) when `MaxIterations` is unset; a **nil** `Tools` becomes `["*"]` ([ADR-0014](docs/adr/0014-subagent-definitions-degrade-never-disable.md) — an omitted `tools:` key inherits the inventory, while an explicit `[]string{}` still means no tools); and `excludeTodoTools` (appends `-todoread`/`-todowrite` if not already present) for any `Mode == ModeSubagent` config — so a custom subagent never sees the primary agent's todo tools even if its frontmatter didn't think to exclude them.

### Mode taxonomy

The `Mode` field on `AgentTypeConfig` controls visibility and usage:

| Mode | Meaning | Appears in task tool enum | Example |
|------|---------|--------------------------|---------|
| `ModePrimary` (`"primary"`) | Top-level agent, created by daemon | No | `build` |
| `ModeSubagent` (`"subagent"`) | Spawnable via task tool | Yes | `general`, `explore`, custom |
| `ModeHidden` (`"hidden"`) | Internal-only, never exposed to LLM | No | `compaction` |

`Set.ListSubagents()` returns only types with `Mode == ModeSubagent`, sorted by name — this list populates the task tool's type enum shown to the LLM for delegation decisions.

### Tensions

- **A project subagent silently shadows a built-in of the same name.** `NewSet` overlays project configs onto the built-in map by name with no collision warning. This is by design (project-level override), but a project subagent accidentally named `general` or `explore` replaces the built-in without any log line.
- **Tests were added alongside the `Set` refactor.** `registry_test.go` now exists (added in the same commit that introduced `Set`) and covers `NewSet`/`Get`/`Has`/`ListSubagents`/`FilterTools` directly, in addition to the indirect coverage from `session` tests.

### Anti-patterns

- **Don't hold onto a `*Set` across sessions.** Each session builds its own via `NewSet` at construction time from that session's own project context. Sharing a `Set` between sessions with different project-local subagents would leak one project's custom types into another's.
- **Don't use `FilterTools`/`Get` with an unregistered type.** `Get` returns `(zero AgentTypeConfig, false)`; `FilterTools` returns `nil` when the type is unknown — callers must check.
- **Don't put execution logic here.** This package is configuration-only. Agent execution belongs in `session`, tool execution in `tool`/`tool/builtin`. Adding behavior here would create circular dependencies.

### Contracts

- **Project subagents shadow built-in types**: `NewSet` overlays project configs onto the built-in map by name. A project subagent with the same name as a built-in type takes precedence. This is by design — it allows project-level overrides of built-in agent behavior.
- **`ListSubagents` is deterministic**: unlike the pre-July global map, `Set.ListSubagents()` explicitly sorts by name (`slices.SortFunc`). Consumers may depend on ordering now.
- **Prompt constants are consumed by session and compaction**: `BuildAgentPrompt` is the system prompt for primary sessions. `CompactionInitialPrompt`/`CompactionMergePrompt` drive the compaction LLM calls in `session/compaction.go`. `PostCompactionAssistantAck` and `PostCompactionPrimer` are injected as synthetic messages after compaction.
- **`FilterTools` preserves input order**: The filtered result maintains the same relative order as the input `allTools` slice.
- **Setup precedes resolution**: `session.newWithOptions` loads project context (and therefore builds the subagent-config list) *before* calling `NewSet`/`Get` to resolve its own agent type — a project subagent config must exist before the session can resolve against a `Set` that includes it.


<a id="pkg-schedule"></a>

## `internal/schedule`

_Schedule storage, cron validation, one-shot/recurring scheduling, executor, the `schedule` and `sleep` tools._


### Purpose

Provides scheduling service for delayed/recurring task execution. Supports cron expressions (with optional `CRON_TZ=` prefix for timezone-aware scheduling) for recurring schedules and one-shot timestamps for delayed execution. Manages schedule persistence in SQLite and background execution via Executor. Since the July refactor this package also owns the **tool implementations** that used to live in `tool/schedule_tool.go`/`tool/sleep.go`: `scheduleTool` and `sleepTool` are defined here and hold `schedule.Service` directly -- no DI interface needed, since the tool and the service it drives are in the same package.

A cron schedule carries a `fresh` flag. A **fresh** cron schedule delivers only its own prompt and asks the session to wipe its accumulated context first (a blank-slate run each tick, so stale data from earlier runs can't leak in or bloat the context -- state that must survive is persisted to curated memory, which rides in the system prompt, not the transcript). A non-fresh cron tick appends a "Schedule tick #N" header to the live history, preserving context (the default -- e.g. stand-ups that build on prior runs). An exact sleep is a one-shot row carrying `metadata.tool_call_id` and is never fresh; a metadata-free one-shot is standalone scheduled input and follows its own `fresh` flag.

### Key types

| Name | Type | Role |
|------|------|------|
| `Service` | interface | Seven operations: list/remove, recurring-only `AddRecurring`, exact `AddSleep`, `PendingSleeps` recovery/wait projection, pending-sleep cancellation and full session cleanup. Recurring and sleep creation are different method shapes |
| `svc` | struct | `Service` implementation, wraps `Store` |
| `Executor` | interface | Background goroutine that checks and fires due schedules |
| `executor` | struct | `Executor` implementation |
| `Store` | interface | Context-aware SQLite row-persistence contract (11 methods). Its generic `AddSchedule` mirrors the table discriminators; the domain `Service` does not expose that optional variant. `Service.AddSleep` uses `AddScheduleWithMeta` to persist the exact `ScheduleMetadata.ToolCallID`; `RemoveSleepSchedules` deletes only rows with that ownership marker |
| `store` | struct | SQLite implementation of `Store` |
| `Schedule` | struct | Schedule entity (unexported fields, exported accessor methods) -- implements this package's own `Entry` interface, not a `tool`-defined one |
| `Entry` | interface | `ID()`/`CronExpr()`/`OneShotAt()`/`InputMessage()`/`LastFiredAt()`/`Fresh()` -- read-only view of a stored schedule, as read by the `schedule` tool and the read-only `controllerapi.ListSchedules` mapping |
| `Created` | struct | Result of schedule creation (ID, type, next fire time) |
| `ScheduleMetadata` | struct | JSON-serialized metadata (currently only `ToolCallID`) |
| `SessionSender` | interface | Four-method consumer contract: exact pending result, normal tick, fresh task, PubSub notification; implemented by `daemon.Service`, pinned in `main.go` |
| `scheduleTool` | private struct (`tool.Tool` impl, ID `tool.IDSchedule`) | The `schedule` tool: cron CRUD (create/list/cancel) with timezone support; `create` accepts `prompt` + `fresh` (a fresh schedule requires a prompt) |
| `sleepTool` | private struct (`tool.Tool` impl, ID `tool.IDSleep`) | The `sleep` tool: one-shot timer, returns `tool.ErrSuspend` |

Compile-time checks: `var _ Service = (*svc)(nil)`, `var _ Store = (*store)(nil)`, `var _ Executor = (*executor)(nil)`, `var _ tool.Tool = (*scheduleTool)(nil)`, `var _ tool.Tool = (*sleepTool)(nil)`.

### File map

- `service.go` -- `Service` interface + `svc` implementation (CRUD, exact `AddSleep`, `PendingSleep{CallID, WakeAt}` projection, cron next-fire calculation and ownership-aware cleanup)
- `executor.go` -- `Executor` interface + `executor` implementation, `SessionSender` interface (uses `sessionevent.Notification`, so `schedule` never imports `session`), `parseCronTZ` (schedule-time) and exported `SplitCronTZ` (display-time strip of the `CRON_TZ=` prefix, reused by `parseCronTZ` and the controller's `ListSchedules` mapping)
- `store.go` -- `Store` interface, `Schedule` entity with accessor methods, `ScheduleMetadata`, SQLite persistence, `scanSchedules`, null helpers
- `types.go` -- `Entry` interface, `Created` struct
- `schedule_tool.go` -- `scheduleTool`, `NewScheduleTool(sessionID, svc, loc *time.Location) tool.Tool` -- cron schedule CRUD, embeds the user's timezone name/offset in its `Description()`
- `sleep.go` -- `sleepTool`, `NewSleepTool(svc, sessionID) tool.Tool` -- requires `CallIDFromContext`, persists it via `svc.AddSleep`, then returns `tool.ErrSuspend`
- `export_test.go` -- Exposes `newExecutor` as `NewExecutorForTest` for black-box tests

No `migrate.go` -- schedule table migrations live in `internal/migrate`.

### State ownership

| State | Location | Source of truth | Synchronization |
|-------|----------|-----------------|-----------------|
| Schedules | SQLite `schedules` table | DB | SQLite locking |
| Executor loop | In-memory goroutine | -- | Context cancellation |
| Executor cancel func | `executor.cancel` | -- | Set once in `Start()` |
| One-shot retry attempts | `executor.oneShotAttempts` map | In-memory bounded retry counter; schedule row remains durable source of work | Single executor goroutine |

### Concurrency model

- **Executor goroutine**: Spawned via `Start(ctx)`, runs `loop()` until context cancelled
- **Executor loop**: Polls DB every 10 seconds via `time.NewTicker`, calls `tick()` which runs `fireOneShotSchedules` then `fireCronSchedules`
- **Immediate first tick**: `tick()` is called once before entering the ticker loop, so due schedules fire immediately on startup without waiting for the first interval
- **No mutexes in Executor**: the loop and `oneShotAttempts` retry map are single-goroutine, serial state
- **Store**: Stateless -- SQLite handles concurrency via its own locking

```
Executor.Start(ctx)
    | (goroutine)
Executor.loop(ctx)
    +-- tick(ctx) immediately
    +-- select:
        +-- ticker.C (10s) -> tick(ctx)
        |   +-- fireOneShotSchedules(ctx, now) -> deliver exact sleep result or standalone input -> notify session -> remove schedule
        |   +-- fireCronSchedules(ctx, now)    -> deliver typed tick -> update last_fired -> notify session
        +-- ctx.Done() -> return
```

### Lifecycle (manual composition root)

`NewExecutor(store Store, sender SessionSender) Executor` takes no lifecycle parameter -- `main.go` calls `executor.Start(ctx)` explicitly right after construction and registers `func(ctx) error { executor.Stop(); return nil }` as its own named stop closure. There is no separate internal constructor split anymore; `NewExecutor` itself has no side effects (the goroutine starts in `Start`, not the constructor).

#### Initialization order

1. **Store**: Created via `NewStore(db)` -- survives entire process
2. **Service**: Created via `NewService(store)` -- survives entire process
3. **`core.scheduleSender = daemonSvc`** -- pins the daemon to this package's own interface
4. **Executor**: Created via `NewExecutor(store, sender)`, then `executor.Start(ctx)` -- `main.go` owns the goroutine lifecycle

`daemonSvc` (the `SessionSender` implementation) must exist before this pinning line, which is why `main.go` constructs `daemon.New`/`daemon.Start` before `schedule.NewExecutor`.

### SessionSender interface

```go
type SessionSender interface {
    DeliverPendingCallResult(ctx context.Context, sessionID int64, callID, toolName, content string) (applied bool, err error)
    DeliverScheduleTick(ctx context.Context, sessionID int64, deliveryID, content string) (applied bool, err error)
    DeliverFreshSchedule(ctx context.Context, sessionID int64, deliveryID, content string) (applied bool, err error)
    NotifySession(sessionID int64, n sessionevent.Notification)
}
```

Two responsibilities, with three distinct delivery variants rather than one optional-field/boolean protocol:
- `DeliverPendingCallResult` -- resolves the exact stored sleep call ID; it is never allowed to invent a replacement tool call.
- `DeliverScheduleTick` / `DeliverFreshSchedule` -- deliver one producer-identified occurrence with preserved or reset context respectively. Their receipt distinguishes a newly applied occurrence from an idempotent retry.
- `NotifySession` -- publishes a `sessionevent.Notification` to `daemon.PubSub` (e.g., `NotifyInputReceived` so in-process subscribers like a manager know the session has new input)

One delivery method is called for every due schedule. Delivery waits for actual transcript acceptance and returns `(applied, error)`; `NotifySession` is emitted only when `applied=true`, so producer-ack retry cannot repeat controller output. A child session legitimately holds sleep/cron schedules, and delivery bypasses the root-only PubSub gate, so the child still wakes even though its broadcast is dropped.

### Data flow

#### Adding a schedule
```
scheduleTool -> schedule.Service.AddRecurring(ctx, sessionID, cronExpr, msg, fresh)
sleepTool -> require CallIDFromContext -> schedule.Service.AddSleep(ctx, sessionID, callID, wakeAt, result)
    -> store.AddScheduleWithMeta(ctx, ..., metadata.tool_call_id=callID) -> SQLite INSERT
    <- Created{ID, Type, NextFire}
```

#### Firing one-shot schedules
```
ticker (10s) -> executor.tick() -> store.ListDueSchedules(ctx, now) -> for each due:
    -> metadata.tool_call_id present?
       yes -> sender.DeliverPendingCallResult(sessionID, callID, tool.IDSleep, inputMessage)
       no  -> fresh? sender.DeliverFreshSchedule(...) | sender.DeliverScheduleTick(...)
    -> sender.NotifySession(sessionID, sessionevent.Notification{Type: sessionevent.NotifyInputReceived})
    -> store.RemoveSchedule(ctx, id)  <- row deleted only after accepted delivery
```

#### Firing cron schedules
```
ticker (10s) -> executor.tick() -> store.ListDueCronSchedules(ctx, now) -> for each:
    -> parseCronTZ(cronExpr) -> parse cron -> check if current minute matches
    -> skip if lastFiredAt is in the same minute (dedup)
    -> fresh?  content = inputMessage (prompt only)      | else  content = "Schedule tick #N ...\n\nTask: <msg>"
    -> sender.DeliverFreshSchedule(...) | sender.DeliverScheduleTick(...)
    -> on success: store.UpdateScheduleLastFired(ctx, id, now)
    -> sender.NotifySession(sessionID, sessionevent.Notification{Type: sessionevent.NotifyInputReceived})
```

### Interface dependencies

- `scheduleTool`/`sleepTool` hold `schedule.Service` **directly** -- no DI interface, since the tool lives in the same package as the service it drives. Both import `internal/tool` only for `tool.Tool`/`tool.Result`/`tool.ErrSuspend`/`tool.SleepParams`/`tool.ParseDuration`/`tool.ID*` (the pure protocol leaf).
- `SessionSender` -- consumer-defined interface with four methods, implemented by `daemon.Service` and pinned at the composition root by assigning it to `core.scheduleSender`. `schedule` source files never import `daemon`.

### Ordering constraints

- **Executor.Stop() waits for loop exit**: `e.cancel()` sets context cancelled, loop exits, then closes `done` channel. Caller blocks on `<-e.done` to ensure clean shutdown.
- **Executor.Stop() is safe to call twice**: The second `<-e.done` returns immediately since the channel is already closed.
- **Store must be initialized before Service**: Service wraps Store, no lazy initialization.
- **`daemonSvc` must exist before the `SessionSender` pin, which must exist before `NewExecutor`**: `main.go` enforces this by construction order (see Initialization sequence) -- there is no dependency-injection framework to enforce it automatically anymore.

### Tensions

- **Polling vs push**: Executor polls DB every 10s. Inefficient for precise timing (one-shot schedules may fire up to 10s late). Could use in-memory timer per schedule, but that complicates crash recovery. Current approach favors simplicity and persistence over precision.
- **Acceptance precedes acknowledgement for both schedule kinds**: Cron updates `last_fired_at` only after the session accepts the tick; one-shot deletes its row only after either the exact sleep result or standalone scheduled input is accepted. Standalone occurrences carry deterministic identities, so a crash/failure between acceptance and acknowledgement retries as `applied=false` and produces neither another transcript mutation nor another controller publication. A pending blocking/config call or transient transcript failure remains retryable rather than silently consuming the occurrence.
- **Malformed schedule metadata is fatal for the row scan.** `scanSchedules` returns a wrapped error when metadata JSON cannot be decoded. This keeps corrupted persistence visible instead of silently dropping schedule metadata.

### Package anti-patterns

- **Don't call `Start()`/`Stop()` more than once without a matching pair**: `main.go` owns exactly one `Start(ctx)` / stop-closure pair. Tests use `NewExecutorForTest` for manual lifecycle control but must follow the same one-Start-one-Stop discipline.
- **Don't conflate pending sleeps with standalone one-shots**: both use `one_shot_at`, but only a row with `metadata.tool_call_id` owns a suspended call and appears in `PendingSleeps`. Domain interruption and `/stop` use `CancelPendingSleeps`; the generic store-only `RemoveOneShotSchedules` primitive is reserved for deliberately deleting every one-shot variant.
- **Don't add a `tool.ScheduleStore`-style DI interface back here.** The `schedule`/`sleep` tools hold `Service` directly now; reintroducing a `tool`-defined interface would resurrect the pre-July laundering pattern this refactor removed.

### Contracts

- **Cron schedules never expire**: Unlike one-shot, cron schedules persist forever. Caller must delete explicitly via `RemoveSchedule()`.
- **One-shot schedules are deleted after firing**: The executor calls `store.RemoveSchedule(id)` after successfully sending the notification. Unlike cron (which updates `last_fired_at`), one-shot rows are removed entirely.
- **Cron deduplication**: A cron schedule won't fire twice in the same minute -- `fireCronSchedules` checks if `lastFiredAt` truncated to minute equals the current minute.
- **Cron timezone support**: Cron expressions can be prefixed with `CRON_TZ=<tz> ` (e.g., `CRON_TZ=Europe/Berlin 0 9 * * *`). Parsed by `parseCronTZ()`, falls back to UTC on invalid timezone. `SplitCronTZ()` strips the prefix for display without loading the location.
- **Fresh schedules run from a blank slate**: a `fresh=true` cron tick or standalone one-shot delivers only its stored prompt through the dedicated `DeliverFreshSchedule` variant, driving `session.ResetContextAndInjectOnce` (context wiped, prompt injected as the new task). The `schedule` tool rejects `create` with `fresh=true` and no prompt. Exact sleep rows are always non-fresh.
- **Schedule validation**: The domain service exposes separate `AddRecurring` and `AddSleep` variants; empty cron and empty sleep call ID are rejected before persistence. The lower-level store still validates its row discriminator (`cronExpr` or `oneShotAt`). Cron parsing uses `robfig/cron/v3` with 5-field format.
- **Session-scoped removal**: `RemoveScheduleForSession` enforces ownership -- a session can only delete its own schedules (WHERE clause includes both `id` and `session_id`).
- **`RemoveAllForSession` is daemon-triggered, not tool-triggered**: Only `daemon.svc.removeSchedules` (called from `Kill` and `killSubagent`) calls `Service.RemoveAllForSession`. It is a separate call from `sessionstore.OrchestrationStore.MarkSessionKilled` -- the two are NOT in one transaction (see the daemon section's Kill data flow).
- **Executor acknowledges only accepted input**: Typed delivery errors are logged and left retryable (one-shot retained; cron `last_fired_at` unchanged). `NotifySession` remains fire-and-forget and happens only for a newly applied delivery, not a successful idempotent retry. Cron delivery ID, rendered timestamp and fingerprint share the same minute-truncated occurrence.

### Test patterns

- **Black-box tests** (`schedule_test` package): Executor tests use `NewExecutorForTest` from `export_test.go` to get manual lifecycle control
- **`mockSender`**: Thread-safe mock implementing `SessionSender` with mutex-guarded call recording
- **Real SQLite**: Tests use `migrate.OpenDB` + `migrate.Run` on a temp dir for full schema -- no mocking of the store layer
- **Cross-package stores**: `newTestDB` returns `schedule.Store`, `daemon.Store`, and `sessionstore.Store` from the same DB to set up realistic foreign key relationships (sessions require projects)


<a id="pkg-loader"></a>

## `internal/loader`

_CLAUDE.md parsing, SKILL.md loading, subagent definitions, marketplace git cloning._


### 1. Purpose

Loads and merges configuration artifacts -- context files (AGENTS.md, CLAUDE.md, CLAUDE.local.md), SKILL.md skill definitions, and subagent `.md` definitions -- from a layered set of filesystem paths (marketplace, global, project-local) with priority-based overwriting. The package owns no persistent state; it reads the filesystem on each `Load*()` call and holds results in memory for the duration of a session.

### 2. Key types

| Type | Exported | Role |
|------|----------|------|
| `Loader` | yes | Interface -- setup-time operations: `ProcessMarketplace`, `ProcessMarketplaces` (batch wrapper), `LoadAgentsMD`, `LoadSkills`, `LoadSubagents` |
| `Registry` | yes | Interface -- runtime queries: `GetSkill`, `ListSkills`, `ListUserInvocableSkills`, `ListModelInvocableSkills`, `RegisterSkill`, `GetSubagent`, `ListSubagents`, `RegisterSubagent` |
| `Service` | yes | Interface -- union of `Loader` + `Registry`. Used by code that holds the full object (session params, factory) |
| `svc` | no | Implementation of `Service`. Holds skills/subagents maps and marketplace source path accumulators |
| `Skill` | yes | Parsed SKILL.md: YAML frontmatter (name, description, independent `user-invocable` and `disable-model-invocation` flags) + markdown body. `Content` and `Path` are excluded from YAML. Both visibility axes default to enabled; `AnnouncementDescription()` caps model-facing descriptions at 1,536 Unicode code points |
| `Subagent` | yes | Parsed subagent `.md`: YAML frontmatter (name, description, tools, model) + markdown prompt. `Prompt` and `Path` excluded from YAML |
| `sourceInfo` | no | Pairs a filesystem path with a plugin name prefix. Used to track where marketplace artifacts were discovered |
| `MarketplaceCache` | yes | Interface for thread-safe TTL cache for resolved marketplace repositories. Daemon-level, shared across sessions |
| `marketplaceCache` | no | Implementation of `MarketplaceCache`. Resolver is bound at construction time |
| `marketplaceCacheEntry` | no | Per-repository: resolved repo path, discovered skill/agent `sourceInfo` slices, the set of plugin names already `scanned` against that path, an `err` (a remembered resolve failure — the negative cache), and the last pull timestamp. Immutable once stored: a re-scan clones it |
| `cacheResult` | no | What a fresh entry answers a `lookup` with: skills, agents, and a remembered `err` |
| `RepositoryResolver` | yes | `func(url string) (string, error)` -- resolves a marketplace URL to a local filesystem path via git clone/pull |
| `pluginManifest` | no | JSON manifest from `.claude-plugin/plugin.json` inside a marketplace plugin directory. Fields: name, version, description, author, keywords, agents |
| `pluginAuthor` | no | Author sub-object of `pluginManifest` (name, email) |

### 3. File map

- **`loader.go`** -- `Loader`, `Registry`, `Service` interfaces, `svc` struct, `New()` constructor
- **`types.go`** -- `Skill`, `Subagent`, `sourceInfo` data types; `parseFrontmatterFile()` shared frontmatter/content splitter
- **`paths.go`** -- All filesystem path resolution functions: `globalDir`, `projectDir`, `globalSkillsDir`, `projectSkillsDir`, `globalAgentsDir`, `projectAgentsDir`, `projectCoagentSkillsDir`, `projectCoagentAgentsDir`, `projectCommandsDir`, `projectAgentsSkillsDir`, `contextFilePaths`
- **`agents.go`** -- `LoadAgentsMD()` -- reads and concatenates context files (AGENTS.md, CLAUDE.md, CLAUDE.local.md) from layered paths via `contextFilePaths()`
- **`skill.go`** -- `LoadSkills()`, `loadSkillsFromPath()`, `parseSkillFile()`, `GetSkill`, `ListSkills`, `ListUserInvocableSkills`, `ListModelInvocableSkills`, `RegisterSkill`
- **`subagent.go`** -- `LoadSubagents()`, `loadSubagentsFromPath()`, `parseSubagentFile()`, `GetSubagent`, `ListSubagents`, `RegisterSubagent`
- **`marketplace.go`** -- `ProcessMarketplace()`, `ProcessMarketplaces()` (loops over entries, warn-logs and continues past a failing one via `logger.Named("loader")`), `processMarketplaceWithCache()`, `processMarketplaceDirect()`, `processPlugin()` (standalone, unexported) -- resolves marketplace entries to `sourceInfo` paths; also contains `parseGitHubURL()`, `cacheDirForMarketplace()`, `RepositoryResolver` type
- **`marketplace_cache.go`** -- `MarketplaceCache` interface, `marketplaceCache` implementation (`Resolve`, `scan`, `lookup`, `reusable`, `ttlFor`, `store`, `lockURL`), `NewMarketplaceCache()`, `newMarketplaceCache()`, `NewRepoResolver()`, `defaultMarketplaceTTL` / `failedMarketplaceTTL` consts
- **`marketplace_entry.go`** -- `marketplaceCacheEntry` and `cacheResult` types plus `covers()`, `clone()`, `filterByPlugins()` -- the cache's stored value and the pure functions over it
- **`plugin.go`** -- `pluginManifest`, `pluginAuthor` types, `parsePluginManifest()` -- JSON parsing for marketplace plugin manifests (`.claude-plugin/plugin.json`)

### 4. State ownership

- **`svc.skills`** (`map[string]*Skill`) -- in-memory only. Fully rebuilt (map reassigned) on every `LoadSkills()` call. No synchronization -- single-goroutine access assumed.
- **`svc.subagents`** (`map[string]*Subagent`) -- same pattern as `skills`: rebuilt on every `LoadSubagents()`.
- **`svc.marketplaceSkillPaths` / `svc.marketplaceAgentPaths`** (`[]sourceInfo`) -- accumulated by `ProcessMarketplace()` calls. Append-only, never cleared. These are consumed as the first (lowest-priority) entries in `LoadSkills()` / `LoadSubagents()` search sources.
- **`svc.marketplaceCache`** (`MarketplaceCache`) -- optional, set once at construction via `New(cache)`. When present, `ProcessMarketplace` delegates to cache instead of direct resolution. Not owned by `svc` -- daemon-level lifecycle.
- **`marketplaceCache.entries`** (`map[string]*marketplaceCacheEntry`) -- protected by `marketplaceCache.mu` (`sync.Mutex`). Keyed by marketplace URL. An entry caches either a success (repo path + discovered source paths + the plugin names scanned for them) or a failure (`err`). Stored entries are treated as immutable: a re-scan clones the entry, mutates the clone, and stores it, so a concurrent reader holding the old pointer never observes a half-appended slice. On TTL expiry the resolver is called again and the entry is replaced; a failure entry expires on `min(ttl, failedMarketplaceTTL)`.

### 5. Concurrency model

The `svc` struct is **not thread-safe**. No mutex, no atomics. Designed for single-goroutine use within a session's setup phase.

`marketplaceCache` is the only concurrent-safe type. `Resolve()` holds `mu` only for the short entries lookup/store (`lookup`/`reusable`/`store`); the slow resolve (git clone/pull + `processPlugin()`) runs under a per-URL lock (`keyLocks sync.Map`, `lockURL`) with `mu` released. Same-URL resolves dedupe; different URLs proceed in parallel, so one hung remote can't wedge every other session's `Resolve`. A plugin re-scan mutates a clone (`reusable` returns one) rather than the stored entry, so `mu` protects only the map -- the values themselves are never written after being stored.

`marketplaceCache.now` is an injectable clock (`func() time.Time`) for testing TTL expiry without real time.

No goroutines are spawned by this package.

### 6. Data flow

#### Two-phase pattern: Loader -> Registry

Setup code populates in-memory state via `Loader` methods, then runtime code queries via `Registry` methods.

#### Loading skills for a session
```
ProcessMarketplace(entry, resolver)
    -> [with cache]  MarketplaceCache.Resolve(entry) -> filterByPlugins() -> return sourceInfo slices
    -> [without cache]  processMarketplaceDirect() -> processPlugin() per plugin -> append to svc.marketplace*Paths

LoadSkills(workDir)
    -> build searchSources [marketplace paths + global + .agents/skills + .coagent/skills + .claude/commands + .claude/skills]
    -> loadSkillsFromPath() per source -> parseSkillFile() -> parseFrontmatterFile() + yaml.Unmarshal
    -> svc.skills[name] = skill  (later source overwrites earlier -- priority by position in searchSources)

ListSkills() / GetSkill(name)  ->  reads from svc.skills, sorts by name
```

#### Loading subagents for a session
```
LoadSubagents(workDir)
    -> build searchSources [marketplace paths + ~/.coagent/agents + .coagent/agents + .claude/agents]
    -> loadSubagentsFromPath() per source -> parseSubagentFile() -> parseFrontmatterFile() + yaml.Unmarshal
    -> svc.subagents[name] = agent  (later source overwrites earlier)

ListSubagents() / GetSubagent(name)  ->  reads from svc.subagents, sorts by name
```

#### Loading context files (AGENTS.md / CLAUDE.md)
```
LoadAgentsMD(workDir)
    -> contextFilePaths(workDir) -> returns ordered list:
        1. ~/.coagent/AGENTS.md           (global agents)
        2. {workDir}/AGENTS.md            (project agents)
        3. {workDir}/CLAUDE.md            (project context)
        4. {workDir}/.claude/CLAUDE.md    (project claude dir context)
        5. {workDir}/CLAUDE.local.md      (local-only context)
    -> os.ReadFile each (skip missing) -> concatenate with "---" separator
```

#### Marketplace with cache (daemon mode)
```
MarketplaceCache.Resolve(entry)
    -> [fresh entry, all requested plugins scanned]  filterByPlugins(cached, entry.Plugins) -> return subset
    -> [fresh failure entry]                         return the remembered error (no resolver call)
    -> [fresh entry, plugins not yet scanned]        clone entry, processPlugin() for the missing names only
                                                     (no resolver call — the repo path is still fresh) -> store
    -> [no entry / expired]                          resolver(url) -> processPlugin() per plugin -> store
    -> [resolver error]                              store a failure entry -> return the error
```

#### Plugin resolution (`processPlugin`)
```
processPlugin(repoPath, pluginName)
    -> try plugins/{pluginName} directory, fallback to {pluginName} at repo root
    -> parse .claude-plugin/plugin.json manifest (must exist)
    -> collect skills/ directory as sourceInfo (if exists)
    -> collect agents/ directory as sourceInfo (fallback to commands/ if agents/ missing)
```

### 7. Lifecycle

1. **Construction**: `New()` optionally with `MarketplaceCache`. Maps initialized empty.
2. **Marketplace registration**: Zero or more `ProcessMarketplace()` calls accumulate `sourceInfo` slices in `marketplace*Paths`.
3. **Loading**: `LoadAgentsMD()`, `LoadSkills()`, `LoadSubagents()` read filesystem, populate in-memory maps. Each call fully rebuilds from scratch (map reassigned). Can be called multiple times.
4. **Direct registration** (optional): `RegisterSkill()` / `RegisterSubagent()` inject individual items into the maps without filesystem loading. Used by tests and any caller that already holds a fully built definition. These bypass the normal filesystem-based loading and priority system -- the caller is responsible for providing fully populated `Skill` / `Subagent` values with names already set.
5. **Querying**: `GetSkill()`, `ListSkills()`, `ListUserInvocableSkills()`, `ListModelInvocableSkills()`, `GetSubagent()`, `ListSubagents()` read from populated maps.

No shutdown, cleanup, or resource release. The loader holds no file handles or connections.

#### MarketplaceCache lifecycle

`MarketplaceCache` has an independent daemon-level lifecycle: created once at daemon startup via `NewMarketplaceCache(gitClient)`, shared across all sessions, entries expire by TTL (`defaultMarketplaceTTL` = 30 minutes).

Two constructors exist:
- **`NewMarketplaceCache(gitClient git.Client) MarketplaceCache`** -- public constructor for production use. Takes a `git.Client` directly and creates its own `RepositoryResolver` internally via `NewRepoResolver(gitClient)`. Uses `defaultMarketplaceTTL`.
- **`newMarketplaceCache(resolver RepositoryResolver, ttl time.Duration) MarketplaceCache`** -- unexported constructor for tests. Accepts a resolver function and custom TTL, enabling tests to inject fake resolvers and control time via the injectable `now` clock.

Individual `Resolve()` calls do not accept a resolver -- it is bound at construction.

### 8. Tensions

1. **Marketplace paths accumulate but skills/subagents reset.** `ProcessMarketplace()` appends to `marketplace*Paths` without clearing them, but `LoadSkills()` / `LoadSubagents()` reset the skills/subagents maps. If `LoadSkills()` is called, then `ProcessMarketplace()` again, then `LoadSkills()` again -- the second load re-processes marketplace sources from *both* `ProcessMarketplace()` calls, potentially loading stale entries from the first batch.

2. **Git op runs under a per-URL lock, not `mu`.** `marketplaceCache.Resolve()` holds `mu` only for the entries lookup/store; the resolver call (git clone/pull) runs under a per-URL lock (`lockURL`) with `mu` released, so a slow git op on one URL no longer blocks `Resolve()` for other URLs. `git.Client.Clone`/`Pull` are themselves bounded (`gitTimeout`, `gitWaitDelay`) with credential prompts disabled.

3. **Parse errors are silently swallowed at the skill/subagent level.** `loadSkillsFromPath` and `loadSubagentsFromPath` `continue` on `parseSkillFile` / `parseSubagentFile` errors. A malformed SKILL.md or subagent `.md` is invisible -- it won't appear in the loaded set and no error is reported to the caller. Directory-level errors are reported (aggregated and returned) but do not stop the scan. The error handling is still inconsistent between "path-level" (reported) and "file-level" (swallowed).

4. **Plugin processing errors are now logged in the cache path, still silent in the direct path.** `marketplaceCache.Resolve` warn-logs (`processing_plugin_failed`) and continues on a `processPlugin` error. `processMarketplaceDirect` -- used only when no `MarketplaceCache` is configured -- still bare `continue`s on the same error with no log line. Production wiring always passes a cache, so this asymmetry rarely bites, but the direct path remains genuinely silent.

5. **`NewRepoResolver` now logs but still ignores git pull errors.** When the repo is already cloned, a failed `gitClient.Pull(cacheDir)` is warn-logged (`marketplace_pull_failed`) and the stale local copy is returned anyway. This is intentional (best-effort update) but means the cache can serve arbitrarily outdated content -- the fix was visibility, not behavior change.

6. **`ProcessMarketplaces` isolates one bad entry from the rest, but not from a bad plugin.** The per-entry warn-log (`processing_marketplace_failed`) means one marketplace's resolution failure no longer skips the entries after it in the same call -- but a single entry with several plugins still stops at the first `ProcessMarketplace` error for that entry (`processMarketplaceWithCache` returns on the first `Resolve` error; only per-plugin errors inside `Resolve` are individually skipped-and-logged).

### 9. Ordering constraints

- **`ProcessMarketplace`/`ProcessMarketplaces` before `LoadSkills` / `LoadSubagents`.** Marketplace source paths must be accumulated before loading, otherwise marketplace skills/subagents won't be in the search sources. Callers with a list of entries should use `ProcessMarketplaces` (both `session.setup.loadMarketplaces` and `daemon.controller.ListSkills` do) rather than hand-rolling the loop.

- **Lower-priority paths first in `searchSources`.** The append order determines override priority -- later sources overwrite earlier ones by map key. For skills: Marketplace -> global (`~/.coagent/skills`) -> `.agents/skills` -> `.coagent/skills` -> `.claude/commands` -> `.claude/skills`. For subagents: Marketplace -> global (`~/.coagent/agents`) -> `.coagent/agents` -> `.claude/agents`. Reordering the search path construction changes which artifact "wins" for a given name.

- **`Load*` before `List*` / `Get*`.** Query methods return whatever was loaded. Calling them before loading returns empty results. Calling them between loads returns stale data from the previous load.

- **`RegisterSkill` / `RegisterSubagent` after `Load*`.** Direct registration writes to the same maps that `Load*` methods reset. Calling `RegisterSkill` before `LoadSkills` loses the registered item because `LoadSkills` reassigns the map. Always register after loading.

### 10. Package anti-patterns

- **Don't call `LoadSkills()` / `LoadSubagents()` from multiple goroutines.** The `svc` struct has no synchronization. The maps are reassigned at the start of each load call -- concurrent access corrupts state.

- **Don't pass `Service` where `Registry` suffices.** Runtime code that only queries loaded artifacts should accept `Registry`, not `Service`. Passing the wider interface obscures whether the caller can trigger loading side effects.

- **Don't assume `ListSkills()` returns the same set across `LoadSkills()` calls with different `workDir` values.** The maps are rebuilt from scratch each time. While `ListSkills()` sorts by name, the set of loaded skills depends on which filesystem paths exist under that `workDir`.

- **Don't treat a non-nil `LoadSkills` / `LoadSubagents` error as "nothing loaded".** It is a joined report of the sources that failed to scan; the rest of the sources did load. Log it and keep using the loaded set -- returning early would reintroduce the truncation the aggregation exists to prevent.

- **Don't rely on `parseSkillFile` / `parseSubagentFile` errors surfacing to the caller.** `loadSkillsFromPath` and `loadSubagentsFromPath` `continue` on parse errors. A broken YAML frontmatter is silently skipped. If you need to know about parse failures, you must check at the individual parse function level.

- **Don't call `NewMarketplaceCache` with a nil `git.Client`.** The constructor immediately passes it to `NewRepoResolver`, which will panic on nil dereference when `Resolve()` is called.

### 11. Contracts

- **`LoadSkills` / `LoadSubagents` return only `error`.** They are purely imperative -- "load from disk into memory". Use `ListSkills()` / `ListSubagents()` after loading to access results.

- **A source that fails to scan is skipped, not fatal.** Both loaders walk every search source; a directory error that is not `os.ErrNotExist` (a regular file where a skills dir is expected, `EACCES`, ...) is collected and the scan continues to the higher-priority sources. The return value is `errors.Join` of those failures, so the returned error is a *report*, not a signal that nothing was loaded -- the loaded set is always as complete as the filesystem allowed. Both callers (`session.loadProjectContext` warn-logs, `daemon.controller.ListSkills` discards) rely on this: an earlier version aborted the loop and silently dropped every project skill behind one broken low-priority directory.

- **A failed marketplace resolve is cached.** `Resolve` stores a failure entry and replays the same error for `min(ttl, failedMarketplaceTTL)` (1 minute), so a dead remote costs one git timeout per window instead of one per session create (and per subagent spawn) on the serialized per-URL lock. Callers still see the error on every call, so the existing `processing_marketplace_failed` warning path is unchanged.

- **The loaded plugin set is a function of the config, not of cache warm order.** Two config entries with the same URL and different `plugins:` lists both resolve fully: the entry records which plugin names it was scanned for, and a request naming an unscanned plugin re-scans it against the already-cached repo path without a new clone/pull.

- **`RegisterSkill` / `RegisterSubagent` are simple map inserts.** They overwrite any existing entry with the same name. No validation, no frontmatter parsing, no plugin prefix logic -- the caller provides a fully formed `*Skill` / `*Subagent` with `Name` already set.

- **Map key = display name with prefix.** A skill/subagent's map key is its final prefixed name (`pluginName:skillName` for marketplace, `skillName` for local). `GetSkill(name)` callers must use the prefixed name.

- **`ListSkills()` / `ListSubagents()` must sort results.** Map iteration is non-deterministic. Unsorted results would change tool descriptions on every call and break LLM prompt cache.

- **User and model visibility are independent.** `user-invocable: false` removes a skill from direct-user listings without hiding it from the model. `disable-model-invocation: true` removes it from model announcements/execution without disabling a direct `/skill` invocation. Missing fields enable both paths.

- **`parseFrontmatterFile` returns `nil` frontmatter when file has no `---` delimiters.** Callers check `len(frontmatter) > 0` before YAML unmarshal. An opening `---` without a closing `---` causes all subsequent content to be treated as frontmatter (scanner state machine doesn't require balanced delimiters).

- **Skills support both directory and file layouts; subagents support only files.** `loadSkillsFromPath` handles `skills/foo/SKILL.md` (directory with `SKILL.md` inside) and `skills/foo.md` (flat file). `loadSubagentsFromPath` only handles flat `.md` files -- directories are skipped.

- **Skill/subagent names fall back to filesystem name.** If the YAML frontmatter has an empty `name` field, the name is derived from the directory name (for directory-style skills) or filename without `.md` extension (for file-style skills/subagents).

- **Plugin manifest is required.** `processPlugin` requires `.claude-plugin/plugin.json` to exist and parse as valid JSON. Missing or invalid manifests cause the entire plugin to be skipped.

- **Plugin directory lookup has fallback.** `processPlugin` tries `plugins/{name}` first, then falls back to `{name}` at repository root. Agent directories try `agents/` first, falling back to `commands/`.

- **`NewMarketplaceCache` owns resolver creation.** The public constructor takes `git.Client` and builds its own `RepositoryResolver` via `NewRepoResolver`. Callers never pass a resolver directly -- that path is reserved for tests via `newMarketplaceCache`.

- **`defaultMarketplaceTTL` is a package-level const (30 minutes).** The public constructor uses it; tests can override via `newMarketplaceCache`. `failedMarketplaceTTL` (1 minute) is the negative-cache window and is not configurable -- it is clamped to the success TTL so a short test TTL still bounds it.


<a id="pkg-todo"></a>

## `internal/todo`

_In-memory task list for the agent._


### Purpose

Provides in-memory todo list management for agent sessions. Supports add, update, delete, list operations with priority levels and status tracking. Used by the agent to maintain task lists during execution.

### Key types

| Name | Type | Role |
|------|------|------|
| `Service` | interface | Todo CRUD operations contract |
| `svc` | struct | In-memory implementation with RWMutex |
| `Item` | struct | Todo entity (id, content, status, priority, timestamps) |
| `Status` | type | Enum: pending, in_progress, completed, cancelled |
| `Priority` | type | Enum: high, medium, low |

Compile-time check: `var _ Service = (*svc)(nil)`.

### File map

- `todo.go` — `Service` interface + `svc` implementation with all methods
- `types.go` — `Item`, `Status`, `Priority` type definitions
- `todo_test.go` — comprehensive tests

### State ownership

| State | Location | Source of truth | Synchronization |
|-------|----------|-----------------|-----------------|
| Items | `svc.items` (map) | In-memory | `svc.mu` (RWMutex) |

No persistence — items are ephemeral per session lifetime.

### Concurrency model

- No background goroutines
- All operations are synchronous, no context parameters
- `svc.mu` protects all map operations:
  - `Write` operations (Add, Update, Delete, Replace, Clear): `mu.Lock()` (exclusive)
  - `Read` operations (Get, List, Count): `mu.RLock()` (concurrent reads allowed)
- All locks use `defer Unlock()` — no IO under lock (map operations only), consistent with HLA rule 4

### Data flow

```
session.Service → todo.Service.Add("task", PriorityHigh) → svc.items[id] = item
session.Service → todo.Service.List() → []*Item (copies)
session agent tool → todowrite → todo.Service
```

### Lifecycle

Created fresh per session via `New()`. Each session gets its own `todo.Service` instance. No persistence — items lost when session ends.

### Partial update semantics

`Update()` treats empty strings as "no change" — it only overwrites `Content`, `Status`, or `Priority` when the corresponding argument is non-empty. This allows callers to update a single field without knowing the current values of the others. `UpdatedAt` is always bumped regardless of which fields changed.

`Replace()` is a full swap: it clears the entire map and rebuilds from the input slice. It applies defaults for missing fields (`Status` → `StatusPending`, `Priority` → `PriorityMedium`, `ID` → generated, `CreatedAt` → now if zero) and always sets `UpdatedAt` to now. Input items are defensively copied.

### Tensions

- **No persistence**: Items don't survive session restart. Compare to `schedule` which persists to SQLite. For todo, this is by design — session serializes items to JSON via `persistState()` and restores them on resume, so items survive checkpoints but not process crashes between checkpoints.
- **Return copies vs references**: `Get()` and `List()` return copies to prevent external mutation. But `Add()` returns the pointer stored in the map. Caller could modify it directly, bypassing the lock. Not a bug in practice (callers don't mutate), but inconsistent with the defensive-copy pattern.

### Ordering constraints

- `Replace()` acquires exclusive lock and rebuilds entire map — O(n) operation, blocks all other operations.

### Package anti-patterns

- **Don't use for long-term storage**: Items are in-memory only. Process crash = data loss. Use schedule package if persistence needed.
- **Don't share Service across sessions**: Each session should get its own `New()` instance. Sharing would cause data leakage between sessions.

### Contracts

- **IDs are unique**: `id.Generate()` creates UUIDs, extremely low collision risk.
- **Zero values handled**: `Replace()` sets defaults for empty ID, Status, Priority, CreatedAt.
- **Map iteration order is random**: `List()` returns items in map iteration order (Go randomizes this). No ordering guarantee.
- **JSON serializable**: `Item` has JSON tags on all fields. Session uses this for checkpoint persistence (`todo_items` column in SQLite).

### Test style

Tests use stdlib `testing` only (no testify). Includes table-driven subtests and dedicated concurrent-access tests verifying safety under parallel Add/Update/Delete/Replace/Clear/List.


<a id="pkg-lsp"></a>

## `internal/lsp`

_LSP server lifecycle, code-intelligence queries._


### 1. Purpose

Manages LSP (Language Server Protocol) server processes on behalf of the agent — auto-discovering the right server for a file extension, spawning it lazily, caching the connection per `(serverID, projectRoot)` pair, and exposing a high-level Go API for code intelligence queries (definition, references, hover, diagnostics, call hierarchy, symbols). It also handles pinned lazy installation under `~/.coagent/bin/`: package-manager trees are staged in isolated versioned roots, while direct release artifacts are selected per platform, SHA-256 verified, reduced to one exact executable and atomically published ([ADR-0018](docs/adr/0018-verified-atomic-lsp-installs.md)).

### 2. Key types

| Type | Exported | Role | Owns |
|---|---|---|---|
| `Manager` | yes (interface) | Contract for all LSP operations + `Close()` | — |
| `manager` | no (struct) | Server lifecycle + client cache | `coagentBin string`, `clients map[string]*client`, `keyLocks sync.Map` (per-root start lock), `servers []serverConfig`, `provider shellenv.Provider` (nilable), `mu sync.RWMutex`, `closed bool` |
| `client` | no (struct) | Single JSON-RPC connection to one LSP server process | `ctx`, `cmd`, `stdin/stdout/reader`, `rootPath`, `pending` map (guarded by `pendingMu`), write serialization (`writeMu`), synchronized document content/version map (guarded by `fileMu`), `diagnostics` map (guarded by `diagnosticsMu`), `readLoop` goroutine, `idGen` (atomic) |
| `serverConfig` | no | Declarative config for one language server | `ID`, `Extensions`, `RootFinder` func, `Spawn` func |
| `Request` / `Response` / `Notification` | yes | JSON-RPC wire types | — |
| `ResponseError` | yes | JSON-RPC error, implements `error` interface | — |
| `PublishDiagnosticsParams` | yes | Params for `textDocument/publishDiagnostics` notification | — |
| `FileDiagnostics` | yes | Aggregated diagnostics for one file | — |
| LSP protocol types (`Position`, `Range`, `Location`, `Hover`, `MarkupContent`, `DocumentSymbol`, `SymbolInformation`, `Diagnostic`, `CallHierarchyItem`, `CallHierarchyIncomingCall`, `CallHierarchyOutgoingCall`) | yes | Data transfer types mapping LSP spec | — |

### 3. File map

- **`manager.go`** — `Manager` interface, `manager` struct, `NewManager(provider)`, `getClient` (fast-path cache lookup, then `startClient` under a per-root lock (`keyLocks`/`lockKey`) so the spawn/install + bounded `initialize` (`lspInitTimeout`, 30s) run with `mu` released — `Close` can't be starved; re-wraps `server.Spawn`'s argv through `provider.WrapExec` so the server inherits the project root's activated toolchain), `TouchFile` (delegates document synchronization to the client), `Close()` (collects clients under `mu`, stops them outside it)
- **`manager_ops.go`** — All `Manager` method implementations (Definition, References, Hover, DocumentSymbol, Implementation, PrepareCallHierarchy, IncomingCalls, OutgoingCalls, WorkspaceSymbol, GetDiagnostics, GetAllDiagnostics), `findClientForWorkDir`, helper functions (`positionParams`, `docParams`)
- **`client.go`** — `client` struct, `newClient`, `startWithCommand`, `stop`, `call` (request/response), `notify`, `send` (wire format), `readLoop` goroutine, `cleanupPending`, `handleNotification` (diagnostics ingestion), `getDiagnostics`, `getAllDiagnostics`, helpers (`pathToURI`, `languageID`)
- **`file_sync.go`** — disk-to-server document synchronization: regular-file guard, `ensureFileOpen`/`syncFile`, content deduplication, monotonic versions, full-content `didOpen`/`didChange` parameters
- **`types.go`** — All LSP protocol types (Position, Range, Location, DocumentSymbol, etc.), JSON-RPC types (Request, Response, Notification, ResponseError), `PublishDiagnosticsParams`, `FileDiagnostics`, `FormatDiagnostics`
- **`servers.go`** — private `serverConfig` type, `defaultServers` list (14 languages), and private per-language config factories
- **`root.go`** — nearest project-root discovery shared by server configurations
- **`install.go`** — PATH/legacy discovery, bounded install commands, and the private per-language `findOrInstall*` orchestration
- **`install_specs.go`** — exact Go/npm/gem/release versions, pure package-manager argument builders, and the SHA-256 artifact matrix for `linux|darwin` × `amd64|arm64`
- **`install_stage.go`** — process-wide per-destination install locks, isolated package-manager staging, executable validation, concurrent-winner handling, and atomic file/directory publication; final roots cannot be symlinks, while package-manager binstubs may resolve only within their root; includes the relocatable RubyGems launcher
- **`install_archive.go`** — bounded context-aware HTTP download and compressed-artifact SHA-256 verification
- **`install_extract.go`** — gzip/tar.gz/zip readers that extract only the exact regular executable entry and reject missing, duplicate, linked, or misplaced targets
- **`client_test.go`** — `newTestClient` helper (mock LSP server via `io.Pipe`), `sendNotificationToClient` helper, unit tests for `call`, `notify`, `readLoop` cleanup, diagnostics notification, concurrent calls, `getAllDiagnostics`
- **`manager_test.go`** — Unit tests for `getClient` caching, different roots, extension matching, `Close`, `WorkspaceSymbol`, `GetAllDiagnostics` with limits
- **`integration_test.go`** — Integration tests (build tag `integration`) exercising all operations against a real `gopls` server

### 4. State ownership

- **`manager.coagentBin`** (`string`) — path to `~/.coagent/bin/`, set once in `NewManager`. Passed to `defaultServers` and used as the install target for auto-installed LSP binaries.

- **`manager.clients`** (`map[string]*client`, key `"serverID:root"`) — in-memory only, protected by `manager.mu` (RWMutex). Source of truth for active LSP connections. No persistence — all clients are ephemeral to the daemon process lifetime. Cleaned up by `Close()`.

- **`manager.servers`** (`[]serverConfig`) — immutable after construction. Set once in `NewManager` from `defaultServers(coagentBin)`. Never mutated.

- **`manager.provider`** (`shellenv.Provider`, nilable) — set once in `NewManager`. When non-nil, `getClient` re-wraps each `server.Spawn` command through `provider.WrapExec(ctx, root, cmd.Args, nil)` so the language server spawns with the project root's activated toolchain (PATH/GOROOT). Nil (or no snapshot) → the server spawns exactly as before.

- **`client.ctx`** (`context.Context`) — set once in `newClient`. Used for logging in `readLoop`, `send`, and `handleNotification`. Not used for cancellation — `readLoop` exits on pipe close, not context cancellation.

- **`client.rootPath`** (`string`) — set once in `startWithCommand`. The project root passed to the LSP server during `initialize`.

- **`client.pending`** (`map[int64]chan *json.RawMessage`) — in-memory, protected by `client.pendingMu` (Mutex). Maps request IDs to response channels. Entries added before `send`, removed in `defer` after response or context cancellation. All channels closed by `cleanupPending` when `readLoop` exits.

- **`client.files`** (`map[string]documentState`, key: URI) — in-memory, protected by `client.fileMu` (Mutex). Stores the exact content and monotonically increasing version last accepted by the server. Unchanged disk content emits no notification; first content emits `didOpen` version 1; changed content emits a full-content `didChange` with the next version.

- **`client.diagnostics`** (`map[string][]Diagnostic`, key: URI) — in-memory, protected by `client.diagnosticsMu` (RWMutex). Updated asynchronously by `readLoop` → `handleNotification` on `textDocument/publishDiagnostics`. Read by `getDiagnostics` and `getAllDiagnostics`, which return defensive copies.

- **`client.idGen`** (`atomic.Int64`) — monotonically increasing request ID counter. Lock-free. Starts at 0, first `Add(1)` returns 1.

### 5. Concurrency model

#### Goroutines

- **`readLoop`** — one per `client`, spawned by `startWithCommand`. Reads JSON-RPC messages from server stdout in an infinite loop. Terminates when the pipe closes (server exit) or read error. On exit, `cleanupPending` closes all pending response channels to unblock in-flight callers immediately. Uses `logger.Ctx(c.ctx)` (zap) for structured logging.

#### Mutexes

- **`manager.mu`** (RWMutex) — protects `manager.clients` map and the `closed` flag. RLock for cache lookups in `getClient`, `findClientForWorkDir`, and `GetAllDiagnostics` iteration; Lock only for the short store/clear. The slow spawn+`initialize` runs under a per-root `keyLocks` lock (`lockKey`) with `mu` released, so a hung start can't starve `Close`.
- **`manager.keyLocks`** (`sync.Map`) — per `serverID:root` mutex deduping concurrent starts of the same root while different roots proceed independently.
- **`client.pendingMu`** (Mutex) — protects `client.pending` map only. Acquired briefly for add/lookup/delete operations and cleanup. Independent from `writeMu`.
- **`client.writeMu`** (Mutex) — serializes writes to `client.stdin` via `send()`.
- **`client.fileMu`** (Mutex) — serializes read/compare/notify/update for one client's document states. Lock order is `fileMu → writeMu` because synchronization calls `notify`; `pendingMu` and `diagnosticsMu` are never nested with either.
- **`client.diagnosticsMu`** (RWMutex) — protects `client.diagnostics` map. Write-locked by `readLoop`'s notification handler, read-locked by `getDiagnostics` and `getAllDiagnostics`.

#### Cross-goroutine communication

`readLoop` → caller: via `pending` map channels. `readLoop` looks up the channel for a response ID (under `pendingMu`), sends the result. Caller blocks on `select` with context cancellation. On `readLoop` exit, `cleanupPending` closes all channels, causing blocked callers to receive nil (zero value from closed channel) and return `"no response"` error.

### 6. Data flow

#### LSP query (e.g., Definition)
```
manager.Definition(workDir, file, line, char)
  → manager.getClient(workDir, file)
    → match file extension → serverConfig
    → serverConfig.RootFinder(workDir, file) → root
    → cache lookup (RLock) → miss
    → cache insert (Lock, double-check)
      → serverConfig.Spawn(ctx, root) → *exec.Cmd
      → client.startWithCommand(cmd, root)
        → cmd.Start()
        → go readLoop()
        → call("initialize") → readLoop delivers response
        → notify("initialized")
  → client.ensureFileOpen(file) — synchronizes disk content: didOpen once, then didChange only when changed
  → client.call("textDocument/definition", params) → readLoop delivers response
  → return []Location
```

#### Diagnostics push (async from server)
```
LSP server → stdout → readLoop → json.Unmarshal → handleNotification
  → "textDocument/publishDiagnostics" → diagnosticsMu.Lock → diagnostics[uri] = diags
```

#### Auto-install (first use of a language)
```
serverConfig.Spawn → findOrInstall*(coagentBin)
  → findBinary(PATH, valid legacy coagent path) → miss
  → install lock keyed by final destination
    → package manager: exact version → temporary sibling root
      → validate launch target → rename root into ~/.coagent/bin/lsp/<package>/<version>/
    → direct release: exact OS/arch artifact → bounded temporary download
      → verify compressed SHA-256 → extract exact regular executable
      → enforce 128 MiB executable bound → chmod 0755 → rename into ~/.coagent/bin/
```

### 7. Lifecycle

#### Manager
```
NewManager(provider) → [ready, immutable servers list, empty clients map]
  → getClient() calls lazily spawn servers
  → Close() → stops all clients, clears map → [dead]
```

#### Client
```
newClient() → [empty pending, empty files, empty diagnostics]
  → startWithCommand
    → cmd.Start → readLoop goroutine → call("initialize") → notify("initialized") → [ready]
  → call/notify (operational)
  → stop → call("shutdown") → notify("exit") → Process.Kill → cmd.Wait → [dead]
  → readLoop exit → cleanupPending → [all pending channels closed]
```

### 8. Tensions

1. **`stop()` is kill-first and doesn't wait for the `readLoop` goroutine.** `stop()` kills the process first (so `cmd.Wait()` can't block on a mute server that ignores the graceful RPC), then best-effort sends `shutdown`/`exit` under a 3s `stopTimeout`, then reaps via `cmd.Wait()`. `readLoop` runs in a separate goroutine — it detects EOF and runs `cleanupPending`, but `stop()` returns before this completes. In practice this is safe because `cleanupPending` only touches the `pending` map (under its own lock), but the goroutine's exit isn't joined. Every RPC (`call`) is itself bounded by `lspCallTimeout` (30s), and each auto-install by `lspInstallTimeout` (2 min) + `cmd.WaitDelay`.

2. **`TouchFile` swallows `getClient` errors.** When `getClient` fails (no server for extension, root finder error, spawn error), `TouchFile` returns `nil` instead of the error. All other manager methods propagate `getClient` errors. This inconsistency means callers can't distinguish "file touched successfully" from "no LSP server available for this file type".

### 9. Ordering constraints

- **`initialize` before any request.** `startWithCommand` must complete `call("initialize")` + `notify("initialized")` before any other `call`/`notify`. The LSP spec requires this; violating it causes servers to reject or ignore requests.

- **Synchronize before file-based queries.** All `manager` methods that query a file call `client.ensureFileOpen(file)` before the LSP request. It reads current disk content, emits `didOpen` once or `didChange` when content differs, and updates the remembered state only after a successful notification. `DocumentSymbol` follows the same rule despite not taking a position.

- **`prepareCallHierarchy` before `incomingCalls`/`outgoingCalls`.** `IncomingCalls` and `OutgoingCalls` both internally call `prepareCallHierarchy` first to get a `CallHierarchyItem`, then use it for the actual call. `ensureFileOpen` is called once at the outer method level.

### 10. Package anti-patterns

- **Don't assume `getClient` is free after first call.** `getClient` spawns a server process and performs LSP initialization synchronously on first call for each `(serverID, root)` pair. This can take seconds (especially if auto-installing a binary). Callers with tight timeouts will hit context cancellation.

- **Don't use `GetAllDiagnostics` expecting real-time results.** Diagnostics arrive asynchronously via `readLoop` → `handleNotification`. After a `didOpen`, there's a race between the LSP server computing diagnostics and the caller reading them. `GetDiagnostics` has a hardcoded `100ms` sleep, but `GetAllDiagnostics` reads whatever is currently cached with no wait.

### 11. Contracts

- **One client per (serverID, root) pair.** The double-check locking in `getClient` ensures exactly one `client` (and one server process) exists per key. Creating a second client for the same pair would spawn a duplicate server process with no coordination.
- **One ordered document stream per URI.** `fileMu` makes concurrent `TouchFile` and query synchronization observe one content/version sequence. Duplicate content is suppressed; a failed notification does not advance the remembered version.

- **`readLoop` cleans up on exit.** When `readLoop` exits (pipe broken, process crash), `cleanupPending` closes all pending channels, immediately unblocking any in-flight `call` operations with a nil response (which translates to `"no response"` error).

- **Diagnostics are eventually consistent, not request-response.** `client.diagnostics` is populated asynchronously by server push notifications. Any code reading diagnostics must accept that the data may be stale, empty, or from a previous file version.

- **Auto-install publishes once per destination.** A process-wide keyed lock serializes installs across the per-session LSP managers. Work happens in a temporary sibling; the final root or executable is absent until one rename. A waiting installer accepts the winner only if the expected target is a regular executable. An invalid existing coagent-owned destination is never deleted or replaced automatically — installation fails with a path the operator can inspect and remove.
- **Auto-install has three trust classes.** A user-provided `PATH` executable is accepted as-is. Go/npm/RubyGems clients install exact versions and retain responsibility for registry integrity. Direct release bytes must match the source-pinned SHA-256 before archive parsing begins. Updating a pin or digest is a reviewed source change, not a runtime update check.


<a id="pkg-config"></a>

## `internal/config`

_Environment + unified YAML configuration loading._


### Purpose

Configuration package providing constants, environment-based config loading, and strict unified YAML config parsing. Acts as the configuration layer for the entire application -- defines directory/file name constants, LLM/provider/model config, marketplaces, managers, and tool policy.

Two things it deliberately does **not** define. MCP servers: there is no `mcp:` key; definitions live in SQLite (`internal/mcpstore`), and an old config carrying the section fails on strict unknown-field parsing. Model metadata: `ModelEntry`'s name, limits, pricing and reasoning fields are tagged `yaml:"-"` and filled by `llm.EnrichCatalog` at startup — the YAML carries intent (`id`, `provider`), never metadata.

### Key types

| Name | Type | Role |
|------|------|------|
| `Config` | struct | Top-level config: CLI-only fields + a flat `SubagentModel` override + pointer to `UnifiedConfig`. No subagent-specific credential/base-URL override exists |
| `UnifiedConfig` | struct | YAML config from `~/.coagent/config.yaml` (providers, marketplaces, models, favorites, `projects_root` (the `/new` folder-project root; resolved nil-safely at point of use in the daemon), managers, tools) |
| `ProviderEntry` | struct | LLM provider definition (driver, API key, SA file, base URL, optional `catalog` section name) |
| `ModelEntry` | struct | YAML side: `id`, `provider`, `timeout_sec`, `openrouter_config`. Catalog side (`yaml:"-"`, filled by `llm.EnrichCatalog`): `Name`, `DisplayName`, `ContextWindow`, `MaxTokens`, `Pricing`, `Reasoning` |
| `ModelPricing` | struct | A model's catalog-resolved cost (`InputPrice`, `OutputPrice`, `CacheReadPrice`, `CacheWritePrice`), USD per 1M tokens |
| `ReasoningSpec` | struct | A model's catalog-declared reasoning capability: `Supported`, `NativeEffort` (takes an effort level rather than a token budget), `BudgetMin`, `Efforts` (the allowlist), `AnyEffort` (catalog declares no allowlist), `Default` |
| `ModelEntry.EffortLevels` | []string | The levels the `/model` picker offers for this model, weakest first — the catalog allowlist narrowed to what the driver delivers. Empty means no effort step |
| `ModelEntry.DefaultEffort` | string | The level a switch to this model lands on: the catalog's own preference, else medium, else the middle of the list |
| `OpenRouterConfig` | struct | OpenRouter-specific provider routing (only, order) |
| `MarketplaceEntry` | struct | Marketplace URL + plugin list |
| `ManagerEntry` / `ManagerWhisperEntry` | structs | Manager config (Telegram bot token, allowed user IDs, target chat ID, topic naming/emoji defaults) and its whisper-transcription sub-config (provider/model, validated to require an `openai`-driver provider) |
| `ToolsConfig` / `BashToolConfig` / `BashSandboxConfig` | structs | Bash-scoped sandbox enablement and extra writable roots |

### File map

- `config.go` -- directory/file name constants, `Config` struct, `NewConfig() (*Config, Secrets, error)` (the secrets map is *returned*, never stored on `Config` — hanging it off a struct half the codebase holds would put every credential one field access away from a log line), `DefaultModel()` method
- `secrets.go` -- `Secrets` map type, `LoadSecrets()`, `Secrets.Environment()`, the exported `Secrets.Expand` (also used by `session` to resolve MCP server env at acquire time), `resolveSecrets` (the whitelist of config fields allowed to carry a `${VAR}` reference: provider `api_key` and manager `bot_token`, nothing else), and `secretValues` -- collects every resolved credential into `Config.SecretValues` for log redaction
- `unified_types.go` -- unified YAML data types for providers, models, MCP, marketplaces, managers, and tools
- `unified.go` -- strict YAML loading plus manager/provider/model validation (`validateManager`, `validateTelegramManager`, `validateTelegramWhisper`, `applyTelegramDefaults`)

### State ownership

No mutable state. All functions return new values. `NewConfig()` reads `~/.coagent/secrets` into an in-memory `Secrets` map (`godotenv.Read`, which does **not** mutate the process environment), populates a fresh `Config` via `env.ParseWithOptions` over that map overlaid with the real environment, then loads and attaches `UnifiedConfig` from YAML. `LoadUnifiedConfig()` reads and parses a YAML file into a fresh `UnifiedConfig` and resolves `${VAR}` references in it from the supplied `Secrets`.

The `Secrets` map is the only place credentials live before they reach their consumers (`llm` clients, the Telegram manager, MCP server env). Nothing in this package writes them into the environment, which is what keeps them out of every tool subprocess.

### Concurrency model

None -- pure functions, no goroutines, no synchronization needed. `NewConfig()` is called once at startup in `main.go`, before any other component is constructed. The returned `*Config` is passed by value/pointer directly into each constructor call (`daemon.New(..., cfg)`, `session.NewFactory(cfg, ...)`, etc.) and treated as immutable by all consumers -- there is no DI container to inject it through.

### Data flow

```
main.go --> config.NewConfig()
              |-> LoadSecrets()                <-- godotenv.Read(~/.coagent/secrets) into a map; process env untouched
              |                                    missing file -> empty set; malformed file -> fatal
              |-> env.ParseWithOptions(&cfg, Environment: secrets + os.Environ())
              |                                <-- non-secret knobs; the real environment wins on conflict
              |-> LoadUnifiedConfig(DefaultUnifiedConfigFile, secrets)
              |     |-> expand ~ in path
              |     |-> os.ReadFile
              |     |-> yaml.Decoder.KnownFields(true)  <-- unknown keys/trailing documents are fatal
              |     |-> resolveSecrets(secrets)  <-- ${VAR} in api_key / bot_token / mcp env only; undefined is fatal
              |     |-> validate (Anthropic models require max_tokens)
              |     \-> returns *UnifiedConfig
              |-> cfg.UnifiedConfig = unifiedCfg  <-- stitched internally
              \-> returns *Config (complete config tree)

main.go --> cfg is passed directly into every constructor that needs it
             (daemon.New, session.NewFactory, memory.NewCuratedStore, managers.NewRuntime, ...)
```

### Functions

- **`NewConfig() (*Config, error)`** -- unified constructor. Reads `~/.coagent/secrets` into a `Secrets` map, populates `Config` via `env.ParseWithOptions` (`github.com/caarlos0/env/v11`) over that map overlaid with the real environment, then loads `UnifiedConfig` from `~/.coagent/config.yaml` with the same map. If the YAML file is missing, it continues with `UnifiedConfig` set to nil; `main` logs that outcome. Any other YAML error is fatal. Also fills `Config.SecretValues`: secrets-file values plus resolved `api_key`/`bot_token` sinks (≥8 chars, deduped; MCP env skipped — its secret values come from the secrets file and are already covered, inline env values are plain config). Returns a fully assembled `*Config` ready for injection.
- **`Config.DefaultModel() string`** -- returns the first model ID from `UnifiedConfig.Models`, or `""` if no models configured.
- **`LoadSecrets() (Secrets, error)`** -- parses `~/.coagent/secrets` with `godotenv.Read`. A missing file yields an empty set; a malformed one is an error, because silently losing every credential would surface as unrelated `requires "api_key"` validation failures.
- **`Secrets.Environment() map[string]string`** -- secrets overlaid with `os.Environ()`, the real environment winning. Feeds `env.ParseWithOptions` so shell-exported non-secret knobs keep working. This map is never written back to the process environment.
- **`LoadUnifiedConfig(configPath string, secrets Secrets) (*UnifiedConfig, error)`** -- reads YAML config, expands `~` in path, strictly decodes one document, resolves `${VAR}` references from `secrets`, and validates. Still exported for direct use (e.g., testing, where `nil` secrets are fine), but production code uses `NewConfig()` which calls it internally.

### Validation

The YAML decoder rejects unknown fields before semantic validation, so a misspelled safety setting cannot silently select a zero value. `UnifiedConfig.validate()` then enforces manager contracts, provider drivers and credentials, model/provider references, and Anthropic `max_tokens`.

### Lifecycle

`NewConfig()` is called once at startup in `main.go`, before any other component is constructed. The returned `*Config` is threaded manually into every constructor that needs it. After that, the struct is treated as immutable -- no locking needed. There is no two-step stitching in `main.go`; `NewConfig()` handles the full assembly internally.

### Constants

`config.go` keeps only the **project-checkout** layout names; everything under `~/.coagent` is named by `internal/coagenthome`:

- `ProjectConfigDir` (`".claude"`) / `ProjectCoagentDir` (`".coagent"`) -- project-level config directories
- `ContextFileName` (`"CLAUDE.md"`) / `AgentsFileName` (`"AGENTS.md"`) -- context file names
- `LocalContextSuffix` (`".local"`) -- suffix for local (gitignored) context files
- `SkillsDirName`, `AgentsDirName`, `CommandsDirName`, `AgentsConfigDir`, `SkillFileName` -- loader directory/file names
- `DefaultUnifiedConfigFile` (`"~/.coagent/config.yaml"`) -- in `unified.go`, a const expression composed from `coagenthome.DirName`/`ConfigFileName`

### Tensions

- **Provider credentials in YAML**: All LLM provider config (API keys, base URLs, SA files) now lives in `UnifiedConfig.Providers`. Secrets use `${ENV_VAR}` expansion. The only remaining env-based config is the flat `Config.SubagentModel` string (`SUBAGENT_MODEL`) -- subagents otherwise resolve provider/driver through the same per-model lookup as the main session.
- **Home-level path resolution is delegated**: `SecretsFilePath()` and `ExpandPath()` resolve through `internal/coagenthome`; config provides no `~/.coagent` helpers of its own. Project-level constants stay name-only — consumers (loader) compose paths against a cwd.

### Ordering constraints

- `logger.Init()` must run before `NewConfig()` so `main.go` can log the load outcome and effective Bash sandbox state immediately afterward; the config package itself stays a leaf (its only internal import is `coagenthome`).
- `LoadUnifiedConfig()` is called internally by `NewConfig()`. External callers can still use it directly (tests do), but production flow goes through `NewConfig()`.
- Downstream code that reads `cfg.UnifiedConfig` may receive nil if the YAML file was missing. Consumers must handle this (and they do -- `DefaultModel()` checks for nil).

### Package anti-patterns

- **Don't resolve `~/.coagent` paths ad hoc**: `internal/coagenthome` is the single owner — `os.UserHomeDir` is semgrep-banned (`coagent-no-direct-user-home-dir`) outside it, including in tests. Test binaries fail closed if resolution points at the inherited home, an alias, or a descendant; tests must isolate `HOME` under a temporary directory.
- **Don't duplicate model/provider config across env and YAML**: new provider config should go in UnifiedConfig (YAML), referencing `~/.coagent/secrets` via `${VAR}` for the credential fields. The real environment is for non-secret overrides only.
- **Don't bypass `NewConfig()` in production**: the old pattern of calling `GetConfig()` + `LoadUnifiedConfig()` separately and stitching in `main.go` is replaced. Use `NewConfig()` for the complete config tree.

### Contracts

- **Config is immutable after load**: `NewConfig()` returns a pointer but callers treat it as read-only. No thread-safety needed because it's loaded once at startup and never mutated.
- **Secrets never reach the process environment**: `LoadSecrets` uses `godotenv.Read`, not `Load`. Nothing in this package calls `os.Setenv`. This is what keeps credentials out of Bash, MCP and LSP subprocesses, all of which inherit `os.Environ()` — so reintroducing a `godotenv.Load` here silently hands every credential to the agent's shell. There is no CWD `.env` loading either: a task repository must not be able to inject variables into the daemon.
- **`${VAR}` resolves from the secrets file only, with no environment fallback**: a resolved reference therefore proves the value was never environment-visible. Substitution applies only to `providers.*.api_key` and `managers[].bot_token`; every other field stays literal, and a new secret-bearing field requires editing `resolveSecrets`.
- **Substitution runs after YAML parsing, on decoded scalars**: a credential containing `:`, `#` or a newline is inert and cannot alter document structure. Only the braced `${NAME}` form is recognised, so a literal `$` inside an inline key is preserved.
- **An undefined reference is fatal and names the variable**: silently expanding to an empty string pushed the failure into validation, which then reported a misleading `requires "api_key"`.
- **YAML is strict and single-document**: unknown keys and additional documents are fatal. This prevents configuration typos such as `enable` from silently disabling a safety control.
- **Missing YAML config is not an error**: `NewConfig()` treats `os.IsNotExist` from `LoadUnifiedConfig` as a non-fatal condition. The returned `Config` will have `UnifiedConfig == nil`. All other errors (parse failures, validation errors) are fatal.
- **Logging stays in main**: config returns data/errors only. `main.logConfigStatus` reports the file outcome and `bash_sandbox_enabled` without introducing a logger dependency into this leaf.
- **`Config.SecretValues` feeds log redaction**: `main` hands it to `logger.SetRedactedValues` right after `NewConfig()`; config never imports logger, logger never imports config — main bridges. A new secret-bearing config field must be added to `secretValues` alongside `resolveSecrets`.


<a id="pkg-logger"></a>

## `internal/logger`

_Structured logging via zap._


### Purpose

Provides structured logging via `go.uber.org/zap` with a composable `zapcore.Core` pipeline. Features colorful columnar terminal output, session ID prefix injection, context-aware logging, and JSON formatting helpers for tool arguments.

### Key types

| Name | Type | Role |
|------|------|------|
| `L` | `*zap.Logger` (var) | Global logger, pre-initialized before `Init()` |
| `atom` | `zap.AtomicLevel` (var) | Runtime log level control |
| `Option` | `func(*initConfig)` | Functional option for `Init()` pipeline configuration |
| `initConfig` | struct | Accumulates `zapcore.Core` list during `Init()` |
| `sessionPrefixCore` | struct | `zapcore.Core` wrapper: intercepts `session_id`/`agent_id` fields, prepends `[id]` or `[root:agent]` prefix |
| `redactingCore` | struct | `zapcore.Core` wrapper: scrubs registered secret strings from messages and string/byte-string/error fields |
| `redactedValues` | `atomic.Pointer[[]string]` (var) | Registered secrets, sorted longest-first so overlapping secrets leave no residue |
| `humanEncoder` | struct | `zapcore.Encoder` for colorful fixed-width column output using lipgloss |
| `field` | struct | Key-value pair accumulated by `humanEncoder.With()` |
| `sliceEncoder` | struct | `zapcore.PrimitiveArrayEncoder` for collecting array elements as strings |
| `ctxKey` | struct | Context key for logger-in-context pattern |

### Exported functions

| Function | File | Role |
|----------|------|------|
| `Init(...Option)` | `logger.go` | Builds core pipeline from options, replaces global `L` |
| `SetDebug(bool)` | `logger.go` | Toggles between debug and info level via `atom` |
| `Named(string)` | `logger.go` | Returns `L.Named(name)` — shorthand for component loggers |
| `Ctx(context.Context)` | `logger.go` | Extracts logger from context, falls back to `L` (nil-safe) |
| `With(context.Context, ...zap.Field)` | `logger.go` | Returns new context with enriched logger |
| `ToContext(context.Context, *zap.Logger)` | `logger.go` | Stores a logger in context |
| `WithHumanOutput(io.Writer)` | `logger.go` | Option: adds colorful column encoder core |
| `WithJSONOutput(io.Writer)` | `logger.go` | Option: adds JSON encoder core |
| `WithSessionPrefix()` | `logger.go` | Option: wraps all existing cores with session prefix middleware |
| `newSessionPrefixCore(zapcore.Core)` | `core_session_prefix.go` | Private wrapper for session ID prefix logic |
| `SetRedactedValues([]string)` | `core_redact.go` | Registers secret strings scrubbed from every entry; atomic, callable at any time |
| `newRedactingCore(zapcore.Core)` | `core_redact.go` | Private wrapper for secret redaction |
| `Redact(string)` | `core_redact.go` | Scrubs registered secrets from a string — for output that bypasses zap (main's stderr prints) |
| `newHumanEncoder()` | `encoder_human.go` | Creates the private colorful column `zapcore.Encoder` |
| `FormatArgs(json.RawMessage, int)` | `format.go` | Formats JSON tool arguments as compact `key=value` string with truncation |

### File map

- `logger.go` — global logger `L`, `Init()` with functional options, context helpers (`Ctx()`, `With()`, `ToContext()`), `SetDebug()`, `Named()`
- `core_session_prefix.go` — `zapcore.Core` middleware that intercepts `session_id`/`agent_id` Int64 fields, removes them from output, and prepends `[id]` or `[root:agent]` prefix to the message
- `core_redact.go` — secret-redaction middleware: package-global registered secret list (`SetRedactedValues`) + `zapcore.Core` wrapper replacing occurrences with `[REDACTED]`; an error field is flattened to a string field only when its text carries a secret
- `encoder_human.go` — `zapcore.Encoder` with colored fixed-width columns (time, level, component, message, key=value details) using lipgloss; includes `sliceEncoder` for array marshaling
- `format.go` — `FormatArgs()` for JSON tool argument formatting with per-value and total truncation
- `*_test.go` — unit tests for `FormatArgs`, `sessionPrefixCore`, and `humanEncoder`

### Core pipeline

```
Init(WithHumanOutput(os.Stderr), WithSessionPrefix())
  → redactingCore(sessionPrefixCore(humanColorCore(writeSyncer)))
```

`Init()` unconditionally wraps the assembled core (single or tee) with `newRedactingCore`; the bootstrap core built in `init()` is wrapped too, so no `L` incarnation ever writes unredacted. Outermost core (redaction, then session prefix) processes first. The human encoder renders `Entry.LoggerName` as the component column. `Named()` calls accumulate dot-separated names (`daemon.session.toolexec`).

To switch to JSON output: `Init(WithJSONOutput(os.Stderr))`.

### State ownership

- `L` and `atom` are package-level globals. `Init()` replaces `L`; `SetDebug()` mutates `atom`.
- `redactedValues` is a package-level `atomic.Pointer` — `SetRedactedValues()` swaps it; cores read it at `Write()` time, so registration works after `Init()`.
- `sessionPrefixCore` instances are immutable — `With()` returns a new wrapper.

### Concurrency model

- `zap.AtomicLevel` is goroutine-safe for runtime level changes
- `zapcore.Core.With()` returns new instances (immutable pattern) — safe for concurrent use
- `Init()` should be called once at startup before goroutines log

### Data flow

```
logger.Ctx(ctx).Named("tool.bash").Info("executing", zap.String("cmd", cmd))
  → redactingCore.Write() → replaces registered secrets with [REDACTED] in message + fields
  → sessionPrefixCore.Write() → prepends [session:agent] prefix to Entry.Message
  → humanEncoder.EncodeEntry() → "15:04:05 INFO  tool.bash    [67:89] executing           cmd=ls"
  → os.Stderr
```

#### Session prefix logic

When `session_id == agent_id` (root session), the prefix is `[id]`. When they differ (subagent), the prefix is `[session_id:agent_id]`. The prefix is prepended to `Entry.Message` in `Write()`, so downstream encoders see it as part of the message text.

### Lifecycle

1. Package init: `L` created with basic console encoder (debug level), wrapped in `redactingCore`
2. `main.go`: `Init(WithHumanOutput(os.Stderr), WithSessionPrefix())` replaces `L` with full pipeline
3. `main.go`: `SetRedactedValues(cfg.SecretValues)` immediately after `config.NewConfig()` — entries logged before registration are not retro-scrubbed, but config is a leaf that never logs, so no secret exists in log-reachable state earlier
4. `SetDebug` exists but is never called anywhere in production code (confirmed unused) -- the package-level `atom` set by `init()` stays at `zapcore.DebugLevel` for the process lifetime, so the daemon currently runs at debug level unconditionally

### Contracts

- **`L` is usable before `Init()`** — pre-initialized in `init()` with a console encoder at debug level
- **`Ctx(ctx)` never returns nil** — falls back to global `L`; safe to call with `nil` context
- **`Init()` option ordering matters** — `WithSessionPrefix()` wraps cores that already exist in `initConfig.cores`, so it must come after `WithHumanOutput`/`WithJSONOutput`
- **Session prefix core only intercepts `Int64` typed fields** — `zap.String("session_id", ...)` passes through unchanged; only `zap.Int64("session_id", ...)` is intercepted
- **Redaction covers string-carrying field types only** — `String`/`ByteString`/`Error`; numeric and structured types (`Any`/`Reflect`/`Stringer`/marshalers) pass through, so never log a secret inside a struct field
- **`With()` on session prefix core is immutable** — returns new wrapper with copied IDs, safe for concurrent use
- **`FormatArgs` individual value truncation** — values longer than 50 characters are truncated with `…` before the total `maxLen` truncation is applied


<a id="pkg-llmwire"></a>

## `internal/llmwire`

_The LLM wire vocabulary: Message, Response, ToolCall, ToolSchema, MessageUsage, role constants._

### Purpose

`llmwire` is a pure-data leaf holding the request/response shapes exchanged with LLM backends. It imports nothing internal. **It is a carrier, not a participant: types only, no functions.** It moves data across package boundaries and interprets none of it — a field whose *meaning* only one package understands still travels here, but the code that understands it does not. Consumed by `llm` (request/response shape), `tool` (`ToSchemas` output), and `session` (conversation history, persistence). This is what lets `llm` depend on nothing but `config`, `llmwire`, and `logger` — zero dependency on `tool`. It is one half of the former `dto` package; the event half became `sessionevent`.

### Key types

| Name | Role |
|------|------|
| `Message` | Conversation message: `Role`, `Content`, `ToolCallID`, `ToolName`, `ToolCalls`, `ReasoningContent`, `ReasoningRaw`, `CostUSD`, `Usage`. `ReasoningRaw` is an opaque blob — only `llm` seals and opens it. `DBID int64` is session-side persistence bookkeeping only — tagged `json:"-"`, excluded from wire serialization |
| `Response` | LLM's response: `Text`, `Thoughts`, `ToolCalls`, `FinishType`, `ReasoningContent`, `ReasoningRaw`, `CostUSD`, `Usage` |
| `ToolCall` | A tool invocation request from the LLM: `ID`, `Name`, `Arguments` |
| `ToolSchema` | Wire description of a tool sent to an LLM backend: `Name`, `Description`, `Parameters` |
| `MessageUsage` | Token usage for a single message/call: prompt/completion/cache tokens |
| role constants | `RoleUser`/`RoleAssistant`/`RoleTool`/`RoleSystem` |

### File map

| File | Responsibility |
|------|---------------|
| `llmwire.go` | Role constants, `MessageUsage`, `Message`, `Response`, `ToolCall` |
| `chatoption.go` | `ChatOption`/`ChatOptions`/`WithMaxTokens`/`ApplyChatOptions` and `EffectiveMaxTokens`. An option only tightens a request: the effective cap is `min(client, option)`, except that a client limit of 0 means "unset" and the option becomes the limit. Compaction is the sole user — the Anthropic thinking budget is sized against the *effective* cap, or a capped request 400s by construction |
| `toolschema.go` | `ToolSchema` — the crossover point from `tool.Tool` (via `tool.ToSchemas`) into the LLM wire format |

### State ownership & concurrency

No mutable state, no constructors, no behavior — `grep '^func' internal/llmwire/*.go` returns nothing, and that is the invariant. Synchronization is the caller's (e.g. `session.messageStore.mu` around the `[]Message` slice).

### Contracts

- **`Message.DBID` never crosses the wire.** `json:"-"` excludes it from any JSON serialization of a `Message` (LLM requests, transcripts). It exists solely so `session` can correlate an in-memory message with its SQLite row.
- **`ToolSchema` is what `llm.Client.Chat` accepts, not `tool.Tool`.** Conversion happens via `tool.ToSchemas` at the session/daemon boundary, keeping `llm` free of any dependency on `tool`.
- **`ReasoningRaw` is carried sealed.** `session` stores it, `sessionstore` writes it to `messages.reasoning_raw`, and neither looks inside. The `{model, payload}` envelope and its replay rule live in `internal/llm` (`reasoning_envelope.go`), the only package that produces or consumes the payload.

### Package anti-patterns

**Never add a function here.** The pull is real and it has already happened once: `ReasoningRaw` legitimately belongs in `Message` because it travels the whole route (`llm` → `session` → `sessionstore` and back), and `WrapReasoning`/`UnwrapReasoning` followed the field in — even though every caller was in `llm`. **A route is not ownership.** Data crosses this package; the rules about that data belong to whoever acts on it.


<a id="pkg-sessionevent"></a>

## `internal/sessionevent`

_The session→controller event vocabulary: Notification, NotificationType, Notify* constants._

### Purpose

`sessionevent` is the leaf owner of notification shapes and ephemeral runtime state emitted toward built-in managers. It imports nothing internal. Consumed by `session`/`schedule` → `controllerapi`/`daemon` → managers; `llm` has zero consumers of it. It is the event half of the former `dto` package — split out (originally from `session/event.go`) so producers and consumers can share the contract without importing `session`.

### Key types

| Name | Role |
|------|------|
| `Notification` | Discriminated value struct with `Validate`: `Type`, `Message`, `Source` (`user`/`agent`/`scheduler`), plus variant-specific fields (`Name`/`WorkDir`/`Attributes`, typed runtime `Status`/`Reason`, clear IDs, secret request correlation) |
| `NotificationType` | String enum including message/state/input/session lifecycle, the secret request/resolved pair, and structured `NotifyWaiting` |
| `WaitItem` | Structured wait projection: one-shot sleep (`WakeAt`) or foreground subagent (`ChildID`); multiple subagents are currently an `all` set |
| `State` | Ephemeral runtime state enum: `running`/`idle`/`error`; distinct from persisted `sessionstore.SessionStatus` |

### File map

| File | Responsibility |
|------|---------------|
| `notification.go` | `NotificationType`/`State` constants, `Notification`, structural `Validate` |

### State ownership & concurrency

No mutable state or constructors. Values pass between goroutines; `Validate` is pure.

### Contracts

- **`Notification` is a validated discriminated struct.** `Validate` rejects unknown types, missing required fields, invalid runtime states/sources and fields belonging to a different variant. Every production event passes through `daemon.svc.publish`, which validates before DB routing or fan-out; callers still switch on `Type` before reading variant fields.


<a id="pkg-controllerapi"></a>

## `internal/controllerapi`

_The complete `Controller`, narrow `ChatController`, and all request/response DTOs — private leaf contracts for built-in managers._

### Purpose

`controllerapi` is the private in-process control-plane contract. It holds the complete `Controller` aggregate, the narrower `ChatController` capability, every request/response DTO built-in managers exchange with the daemon, and `SessionNotification`. Its `State` and `State*` names alias the runtime vocabulary owned by `sessionevent`; it does not create a fourth state machine. Its sole explicit project edge is `sessionevent`. The daemon implements both structurally; managers program against the smallest applicable contract and never import `daemon`. Its location under `internal/` is part of the product boundary: external controllers and plugins are not supported.

### Key types

| Name | Role |
|------|------|
| `Controller` | Rich in-process interface: create/send/list/stop/kill/clear sessions, configure them, inspect models/skills/schedules/projects, subscribe to events |
| `ChatController` | Narrow CLI capability: create, durably send, list and stop sessions, provision its project, subscribe/unsubscribe |
| `SessionNotification` | Wraps `sessionevent.Notification` with the originating `SessionID` for global fan-out |
| request/response DTOs | Includes `SessionCreateData`, the unified `SessionMessageData`, stop/kill/clear/config DTOs, discovery DTOs and `SessionInfo` |
| `State` + constants | Aliases of `sessionevent.State` and its `StateIdle`/`StateRunning`/`StateError` runtime values |

### File map

| File | Responsibility |
|------|---------------|
| `controller.go` | `ChatController` capability and complete `Controller` aggregate |
| `types.go` | All request/response DTOs + `State*` constants |
| `notification.go` | `SessionNotification` |

### State ownership & concurrency

No mutable state, no behavior — pure contract + data. The implementation and all synchronization live in `daemon`.

### Contracts

- **`daemon` is the only implementor.** `var _ controllerapi.Controller = (*daemon.controller)(nil)`. `NewController` returns the complete interface; CLI narrows the value to `ChatController` at its constructor boundary, while Telegram retains `Controller` because it uses nearly every operation.
- **A narrow harness never grows no-op methods.** Adding an operation outside `ChatController` cannot break CLI. If a future transport needs a genuinely stable subset, add a producer-owned named capability and embed it in `Controller`; do not satisfy the aggregate with dummy methods.
- **No package that imports `controllerapi` may import `daemon`.** The whole point is to keep `daemon` out of every importer but `cmd/coagent`.


<a id="pkg-sessionstore"></a>

## `internal/sessionstore`

_Session + ordered message persistence (SQLite): capability Stores, SessionRecord, StoredMessage, atomic compaction replacement, and the delivery CAS — the schema's sole owner._

### Purpose

`sessionstore` owns `sessions`, `messages`, `session_inbox`, and `session_deliveries`, plus every transaction that crosses those tables and `subagent_links`: initial child+link creation, activation terminalization/re-arm against pending inbox state, completion delivery markers+messages, and scheduled occurrence identity+transcript mutation.

### Key types

| Name | Role |
|------|------|
| `Store` | Complete aggregate embedding `RuntimeStore`, `OrchestrationStore`, and `InboxStore` |
| `RuntimeStore` | Live-session authority: transcript append/load/clear/compaction, atomic context reset, loop/todo/brief checkpoints, tree usage and synthetic tool-notification pairs. It embeds `ScheduledDeliveryStore`; no create/kill/discovery/subagent delivery authority |
| `ScheduledDeliveryStore` | Producer-identified, fingerprint-checked exactly-once tool-notification and fresh-reset transactions over `session_deliveries` + transcript/session state |
| `OrchestrationStore` | Daemon lifecycle authority, including all-session tree discovery and conditional child activation transitions |
| `InboxStore` | Durable FIFO acceptance, explicit resolution (`accepted`, control-plane `handled`, `rejected`, `cancelled`), accepted-message identity lookup, and discovery of pending or active accepted turns after restart |
| `store` | private struct — `Store` implementation over `*sql.DB` |
| `SessionRecord` | Row in `sessions`; `Status` is typed `SessionStatus` rather than a controller/link string |
| `SessionStatus` | Persisted vocabulary: `active`, `completed`, `suspended`, `error`, `stopping`, `stopped`, `terminating`, `killed` |
| `SubagentCreate` | Persistence command for the child session fields plus initial link fields; daemon owns the supplied link vocabulary, sessionstore owns the all-or-nothing transaction |
| `StoredMessage` | Row in the `messages` table, including `ReasoningRaw` — the provider's own reasoning payload in a `{model, payload}` envelope, written once with the assistant message and never mutated |
| `CompactionEntry` | Either a retained existing message ID or a new synthetic message, in canonical rebuilt order |
| `NewStore(db)` | Constructor returning `Store` |

### File map

| File | Responsibility |
|------|---------------|
| `store.go` | `RuntimeStore`/`OrchestrationStore` capability interfaces, complete `Store` aggregate, `store`, `SessionStatus`, session/message CRUD, atomic `CreateSubagentWithLink`, metadata marks, compaction replacement, records and scan helpers |
| `delivery_store.go` | `DeliverCompletionAtomic` (CAS `delivered_at` + completion-message insert + `delivered_msg_id` stamp, all one tx — the at-least-once delivery dedup, doc-commented as the sole writer of those two link columns) + `insertCompletionMessages` |
| `inbox_store.go` / `inbox_store_helpers.go` | FIFO enqueue/peek, atomic promotion into `messages`, deterministic command handling, rejection/cancellation and startup discovery |
| `activation_store.go` | conditional finalization and delivered-link re-arm serialized with pending inbox writes and stopped session state |
| `scheduled_delivery_store.go` | `InsertToolNotificationPairOnce`, atomic `ResetSessionContextOnce`, delivery-identity claim and conflict validation |

### State ownership & concurrency

All state is in SQLite. `*sql.DB` is goroutine-safe; each method opens its own transaction where atomicity matters (`CreateSubagentWithLink`, `ReplaceCompactedMessages`, `DeliverCompletionAtomic`, scheduled delivery/reset methods, `InsertToolNotificationPairOnce`, `MarkSessionKilled`). No package-level mutable state, no in-memory cache. `messages.position` orders a rebuilt active transcript; ordinary concurrently inserted rows leave it NULL and sort after positioned rows by ID. Partial indexes separately back the pending FIFO and accepted-message recovery lookups, so startup and first-run inference do not scan historical resolved input.

### Contracts

- **Consumers receive authority, not the aggregate.** Live transcript code accepts `RuntimeStore`; daemon lifecycle and durable input admission receive separate `OrchestrationStore` and `InboxStore` dependencies. The composition root may supply the same concrete `Store` implementation twice, but neither daemon field exposes the aggregate.
- **Message content is immutable after insert (append-only log).** At runtime the `messages` table is only ever appended to or soft-marked via metadata: `compacted_at` (`MarkCompacted`/`ReplaceCompactedMessages`) and `cleared_at` (`MarkCleared`). There is no runtime content-rewrite path — `UpdateMessageContent` was removed. (One-time schema migrations are the only exception and never touch content: migration `00015` zeroed the `cost_usd` accounting column on historical summary rows.) A cleared tool result keeps its stored content; the session's render layer substitutes a placeholder for `ClearedAt != nil` rows. `LoadActiveMessages` filters `compacted_at IS NULL` and carries `cleared_at` through so the projection is reproducible.
- **`reasoning_raw` is written once, with its assistant message, and never updated.** It holds a provider's own reasoning payload in a `{model, payload}` envelope (migration `00017`); replay is the reader's decision, not the store's. Same append-only rule as `content`.
- **`CreateSubagentWithLink` is the production spawn primitive.** It inserts the child session, initial completion-ledger row, and initial agent-source inbox row in one SQLite transaction. A failure at either dependent insert rolls the whole aggregate back. The row-only `CreateSubagentSession` exists for persistence fixtures.
- **`DeliverCompletionAtomic` is the sole writer of `delivered_at`/`delivered_msg_id`.** It rejects empty message sets and a `(sessionID, childID)` pair that does not match the link's actual parent before consuming the CAS. CAS-plus-insert then runs in one transaction; a crash commits both or neither; a lost CAS inserts nothing. See [Cross-package contracts](#cross-package-contracts).
- **Session updates report absence.** Every update addressed by session ID, including `UpdateSessionStatus`, checks `RowsAffected` and returns `session <id> not found` instead of treating a zero-row write as success.
- **Persisted status is typed and validated.** `SessionStatus` cannot be accidentally supplied where runtime `sessionevent.State` or `daemon.LinkState` is expected; store writes also reject explicitly cast unknown values. `CreateSession` returns `StatusActive`, matching the DB row it just inserted.
- **A root session is persisted as the `build` agent.** `CreateSession` writes `agent_type` explicitly; the column's old schema default was `'general'`, a subagent type whose prompt and tool allowlist (no `todoread`/`todowrite`) are wrong for a root. Migration `00021` relabels pre-existing parentless rows that carry that default, and `00022` rebuilds the table with `agent_type TEXT NOT NULL` and **no default**, so an INSERT that omits the column fails instead of silently minting a subagent.
- **`sessionstore` touches the subagent ledger only inside atomic cross-table commands.** Initial creation, activation finalization/re-arm, and delivery are here; independent link reads and single-table stop/kill/reset writes belong to `daemon.LinkStore`.
- **Inbox promotion is causal, atomic, and restart-discoverable.** A pending row becomes `accepted` in the same transaction that inserts its timestamp-preserving user message, records that row as `accepted_message_id`, and marks the live session `active`; lifecycle guards make a concurrent stop/kill roll the whole transaction back. Retrying an already accepted promotion returns its original message without reopening a later-completed session. Startup discovery returns live pending rows plus active sessions with at least one accepted-message identity. It deliberately follows the accepted turn rather than the last message role: tool calls/results and compaction cannot hide unfinished work, a persisted final assistant can be settled without republishing, and a lone AGENTS.md header or handled/rejected command has no accepted identity and cannot trigger the model. `/status` and `/compact` become `handled` and never enter the model transcript. A *rejected* input (e.g. `/skill unknown`), `/status` and `/compact` all share one rule (`nothingToAnswer`): a boundary command may end the activation only when the transcript owes the model nothing — so a fresh session never sends the provider an empty conversation, and tool results executed just before the command still get their model turn.
- **Scheduled delivery identity and mutation are atomic.** Claiming `{session_id, delivery_id, kind, fingerprint}` and applying its transcript/reset effect commit together. Identical re-delivery is a no-op; identity reuse with another kind or fingerprint returns `ErrDeliveryConflict`. A failed reset insertion rolls back the claim, old transcript, compaction brief and todos together.
- **`ReplaceCompactedMessages` preserves retained IDs.** It positions existing rows in place and inserts only synthetic messages, so external references such as `subagent_links.delivered_msg_id` remain valid.
- **Active transcript ordering is `position IS NULL, position, id`.** Positioned canonical rows load first; rows committed concurrently without a position append afterward in insertion order.


<a id="pkg-coagenthome"></a>

## `internal/coagenthome`

_The single owner of coagent-home resolution: the user's home directory, `~/.coagent`, and the name of everything stored inside it._


### Purpose

Every path under `~/.coagent` starts here. `UserHome()` resolves the current user's home, `Dir()` appends `DirName` (`".coagent"`), `Join()` builds paths beneath it, and the const block names each file/dir the daemon keeps there: `config.yaml`, `secrets`, `daemon.sock`, `daemon.lock`, `daemon.db`, `pending-apply.json`, `projects/`, `bin/`, `cache/catalog`, `cache/marketplaces`, `tg-service-<chatID>.json`. It replaced 17 independent `os.UserHomeDir()` call sites that had four different error postures. In a Go test binary, resolution fails closed if it points at the process's inherited home, a symlink alias, or a descendant; tests must opt into an isolated temporary home.

### Key types

| Name | Type | Role |
|------|------|------|
| `UserHome` | func() (string, error) | Current user's home; honors the test override and rejects non-isolated homes in test binaries |
| `Dir` | func() (string, error) | `~/.coagent` |
| `Join` | func(...string) (string, error) | A path under `~/.coagent` |
| `Override` | func(string) func() | Test hook: selects dir — or forces failure when dir is `""` — until restore; inherited-home paths remain forbidden |

### File map

| File | Responsibility |
|------|---------------|
| `coagenthome.go` | Constants + resolution + override |
| `coagenthome_test.go` | Override semantics, path composition, concurrent access, and fail-closed rejection of inherited-home paths in test binaries |

### State ownership

One package-global override pair (`overrideDir`, `overrideSet`) behind `overrideMu` (RWMutex) — test-only. `startupUserHome` snapshots the inherited home once solely as the deny root for test binaries. Normal resolution still reads `os.UserHomeDir()` per call, so a test's temporary `HOME` is observed without cache invalidation.

### Concurrency model

`UserHome` takes the read lock on the override; `Override` and its restore closure swap under the write lock. Concurrent readers are safe; `Override` itself is not for parallel tests — interleaved restore closures can cross.

### Contracts

- **Sole resolution point**: `os.UserHomeDir` is semgrep-banned (`coagent-no-direct-user-home-dir`) everywhere else, including tests. `internal/install` is not an exception but a different concept — it resolves the install *target* user via `os/user`/`SUDO_USER`, not the process's own home.
- **Tests fail closed on user state**: in a Go test binary, `UserHome` rejects the startup home, canonical symlink aliases, and every descendant. Tests resolving coagent-home paths set `HOME` to `t.TempDir()` or override to a temporary directory; production binaries are unaffected.
- **Names, not paths**: constants are bare path components; composition happens at call sites (`config.DefaultUnifiedConfigFile` and daemon's default projects root are const expressions built from them).
- **Error posture belongs to the caller**: `UserHome`/`Dir`/`Join` propagate; each consumer keeps its own swallow/discard/propagate policy.
- **Project-level `.coagent` is not this package**: a task repo's `.coagent/` context dir is `config.ProjectCoagentDir` — same directory name, different scope (see glossary: **coagent home**).


<a id="pkg-id"></a>

## `internal/id`

_Random ID generation for envelope IDs, tool-call IDs, todo-item IDs, and other non-DB identifiers._


### Purpose

A single function, `Generate()`, producing a random 16-character hex string. Used wherever an identifier is needed that isn't a SQLite auto-increment row ID — tool-call IDs, todo-item IDs, and other in-memory identifiers.

### Key types

| Name | Type | Role |
|------|------|------|
| `Generate` | func() string | Returns a random 16-character hex string (8 random bytes via `crypto/rand`, hex-encoded) |

### File map

| File | Responsibility |
|------|---------------|
| `id.go` | `Generate()` |
| `id_test.go` | Uniqueness/format tests |

### State ownership

No mutable state. `Generate()` reads from `crypto/rand` on every call; no package-level state.

### Concurrency model

None needed. `crypto/rand.Read` is safe for concurrent use; `Generate()` has no shared state to protect.

### Contracts

- **IDs are not sequential and carry no ordering information.** Unlike SQLite auto-increment IDs, `Generate()` output can't be used to infer creation order.
- **Collision probability is cryptographically negligible, not zero.** 8 random bytes (64 bits of entropy) — callers needing a uniqueness guarantee rely on collision probability being negligible at expected scales, not on an explicit collision check.


<a id="pkg-git"></a>

## `internal/git`

_Thin git CLI wrapper: marketplace clone/pull + session worktree isolation._


### Cross-cutting insight

This package is a thin wrapper around the `git` CLI. It intentionally avoids adding complex logic, caching, or state. Each operation is a direct translation to a `git` command. This simplicity is a feature — it means predictable behavior, no synchronization issues, and easy debugging. The tradeoff is process spawn overhead, acceptable for low-frequency operations like marketplace sync.

### Purpose

Provides Git operations for two distinct use cases:
1. **Marketplace plugin management** — clone, pull, check existence, get remote URL (`Client` interface, consumed by `loader`)
2. **Session worktree isolation** — find git root, create worktrees for isolated per-session execution (`WorktreeClient` interface, consumed by `session` and `daemon`)

Thin wrapper around the `git` CLI.

### Key types

| Name | Type | Visibility | Role | Owns |
|------|------|-----------|------|------|
| `Client` | interface | exported | Git operations contract (Clone, Pull, IsCloned, GetRemoteURL) | — |
| `client` | struct | private | CLI-based git implementation | — |
| `WorktreeClient` | interface | exported | Worktree operations contract (FindRoot, CreateWorktree) | — |
| `worktreeClient` | struct | private | CLI-based worktree implementation | — |
| `ErrDestinationExists` | error | exported | Sentinel error for Clone when dest exists | — |
| `ErrNotARepo` | error | exported | Sentinel error when path is not a repo | — |

`var _ Client = (*client)(nil)` — compile-time interface check.

`var _ WorktreeClient = (*worktreeClient)(nil)` — compile-time interface check.

### File map

| File | Responsibility |
|------|---------------|
| `client.go` | `Client` interface, `client` implementation |
| `worktree.go` | `WorktreeClient` interface, `worktreeClient` implementation, `ComputeWorktreePaths` |
| `errors.go` | Sentinel errors (`ErrDestinationExists`, `ErrNotARepo`) |
| `constants.go` | Git command constants (`CloneDepth`, `WorktreeBranchDateFormat`) |
| `client_test.go` | Tests for `Client` with temp git repos |
| `worktree_test.go` | Tests for `ComputeWorktreePaths` |

### State ownership

No mutable state. Both `client` and `worktreeClient` structs are empty — stateless utilities.

### Concurrency model

None. Each method spawns a short-lived `exec.Command` process, waits for completion, returns. No goroutines, no concurrency within the package.

### Data flow

```
Marketplace sync (loader):
  loader.MarketplaceCache → git.Client.Clone() → exec.Command("git", "clone", ...) → filesystem
  loader.MarketplaceCache → git.Client.Pull()  → exec.Command("git", "pull", ...)  → filesystem

Session worktree isolation (session, daemon):
  session/daemon → git.WorktreeClient.FindRoot()       → exec.Command("git", "rev-parse", ...) → string
  session/daemon → git.WorktreeClient.CreateWorktree()  → exec.Command("git", "worktree", ...)  → filesystem
  session/daemon → git.ComputeWorktreePaths()           → pure computation (no exec)
```

### Lifecycle

Created on-demand via `New()` (Client) and `NewWorktreeClient()` (WorktreeClient). No lifecycle management — both are stateless.

### Tensions

- **CLI vs library**: Uses `exec.Command` to call `git` binary. Works, but adds process overhead. Could use `go-git` library for pure Go, but adds dependency. Current approach is pragmatic for simple operations.

### Ordering constraints

- `Clone()` must succeed before `Pull()` on same path (enforced by `IsCloned()` check inside `Pull()`).

### Package anti-patterns

- **Don't call from hot path**: Each operation spawns a new process. Fine for marketplace sync and worktree setup (both low-frequency), bad for per-request operations.

### Contracts

- **Clone fails if destination exists**: Explicit design — prevents accidental overwrites.
- **`Clone`/`Pull` self-bound; other ops caller-timed**: the network ops wrap their `git` subprocess in `gitTimeout` (2 min) + `cmd.WaitDelay` (10s — so a detached `git-remote-https` holding the output pipe can't block `CombinedOutput` past the kill) and set non-interactive env (`GIT_TERMINAL_PROMPT=0`/`GCM_INTERACTIVE=never`/`GIT_ASKPASS=`), so a hung remote or credential prompt fails on a deadline. `IsCloned`/`GetRemoteURL`/worktree ops are local and run under the caller ctx only.
- **CreateWorktree retries on existing branch**: If `-b branchName` fails because the branch already exists, retries with `worktree add <path> <branchName>` (checkout existing branch into new worktree). This is intentional — sessions may target the same repo with a previously created branch name.


<a id="pkg-migrate"></a>

## `internal/migrate`

_SQLite database opening and goose migration runner._


Package `migrate` owns SQLite database lifecycle: opening, pragma configuration, schema migrations, and backup. There is no DI framework -- `main.go` calls `Open()` directly and owns the returned `*sql.DB`'s close via its own named stop closure.

### Files

- **`migrate.go`** — `Open` (composition-root entry point), `OpenDB`, `Run`, `configurePragmas`, `backupDB`
- **`legacy.go`** — no-op placeholders for historical Go migrations 1-6

### Public API

#### `Open() (*sql.DB, error)`

The primary entry point used by `cmd/coagent/main.go`. Plain function, no lifecycle parameter. Sequence:

1. Calls `OpenDB("")` to open the default database (`~/.coagent/daemon.db`)
2. Calls `Run(db, "")` to apply pending migrations
3. If migration fails, closes the DB before returning the error

`main.go` owns the returned `*sql.DB`: it registers `func(ctx) error { return db.Close() }` as a named stop closure right after `Open()` succeeds, which runs during shutdown after the daemon has stopped (see [Shutdown sequence](#shutdown-sequence)).

#### `OpenDB(dbPath string) (*sql.DB, error)`

Opens (or creates) a SQLite database. If `dbPath` is empty, defaults to `~/.coagent/daemon.db` (creates the directory if needed). Applies `_txlock=immediate` plus pragmas through the DSN (`_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)`) so **every** pooled connection gets them, then repeats the pragmas through `configurePragmas` on the initial connection (belt-and-suspenders):

- `_txlock=immediate` — every explicit repository transaction is a write transaction, so it reserves the single SQLite writer before performing its reads. This prevents a deferred read snapshot from failing immediately with `SQLITE_BUSY` when it later tries to upgrade after a concurrent parent/child commit; `busy_timeout` can then wait at transaction begin.

- `journal_mode=WAL` — concurrent reads during writes
- `busy_timeout=5000` — 5s retry on lock contention
- `foreign_keys=ON` — enforce FK constraints

Uses `modernc.org/sqlite` (pure-Go, no CGO).

#### `Run(db *sql.DB, dbPath string) error`

Applies all pending migrations using goose v3. The provider merges:

- **Legacy Go migrations (1-6)**: no-op functions registered via `goose.WithGoMigrations`. These are placeholders for historical data-transformation migrations already applied to all existing databases.
- **SQL migrations (7+)**: embedded from `migrations.FS` (`migrations/*.sql`). The baseline schema is `00007_baseline.sql`.

Migration 11 adds nullable `messages.position` and backfills existing rows with `position=id`, preserving every pre-upgrade transcript's insertion order. Migration 12 adds nullable `messages.cleared_at` (the clearing metadata described in [`sessionstore`](#pkg-sessionstore)) and drops the retired extraction/embedding memory subsystem (`extraction_chunks_fts`, `extraction_chunks`, `extractions`, `memory_meta`). Migration 13 adds `schedules.fresh` (`INTEGER NOT NULL DEFAULT 0`) — pre-existing schedules default to non-fresh.

When `dbPath` is non-empty and pending migrations exist, creates a `.bak` backup before applying. Note: `Open()` passes empty `dbPath` to `Run`, so backup only happens when `Run` is called directly with an explicit path.

### State Ownership

- **No package-level state.** All state lives in the returned `*sql.DB`.
- `main.go`'s named stop closure owns the DB close. No other package should close the DB.
- Pragma configuration is applied once at open time and is connection-scoped (doubly so, via DSN + explicit `PRAGMA` -- see `OpenDB` above).

### Concurrency Model

- No goroutines, no mutexes. All operations are synchronous.
- Migrations run during app startup before any concurrent access to the DB.
- The returned `*sql.DB` is safe for concurrent use (standard `database/sql` pool).
- Explicit transactions begin in immediate mode and therefore serialize at
  `BeginTx`; they must remain short and must never span LLM, network, or other
  unbounded I/O.

### Ordering Constraints

1. `OpenDB` must complete before `Run` (pragmas must be set before migrations).
2. `Run` must complete before any other package uses the `*sql.DB` -- `main.go`'s linear construction order enforces this (every store/service that takes `db` is constructed after `migrate.Open()` returns).
3. `db.Close` runs during shutdown, after the daemon (and therefore all its stores) have stopped -- see [Shutdown sequence](#shutdown-sequence).

### Anti-Patterns

- **Never modify an existing migration.** Once merged, a migration is immutable. Fix-ups go into a new migration file with the next version number.
- **Never call `db.Close` manually** anywhere other than `main.go`'s named stop closure — duplicate or premature closes race with in-flight queries.
- **Never skip `Run` after `OpenDB`** in production code — the schema may be out of date.


<a id="pkg-managers"></a>

## `internal/managers`

_Manager runtime: builds, starts, and stops configured controller-side integrations (currently Telegram)._


### Purpose

Thin orchestration layer between `main.go` and individual manager implementations (currently only `managers/telegram`). Reads `cfg.UnifiedConfig.Managers`, builds one `Manager` per enabled entry via a driver switch, starts them in order, and rolls back (stops) any already-started manager if a later one fails to build or start.

### Key types

| Name | Exported | Role |
|---|---|---|
| `Runtime` | yes (interface) | Configured-manager lifecycle and status contract: `Start`, `Stop`, `RunningIDs`, `StartError` |
| `runtime` | no (struct) | `Runtime` implementation; owns configured dependencies and per-manager outcomes |
| `Manager` | yes (interface, 4 methods) | `ID() string`, `Start(ctx) error`, `Stop(ctx) error`, `Alive() bool` — the contract every driver-specific manager (e.g. `telegram.Manager`) satisfies |

### File map

- `runtime.go` — `Runtime` interface, private `runtime`, `NewRuntime(cfg, controller) Runtime`, `Start(ctx) error` (independently builds/starts each enabled entry and records failures), `Stop(ctx) error` (reverse-order stop, first error wins), `stopManagers` helper, `buildManager` (driver switch: `"telegram"` -> `telegram.New`)
- `types.go` — `Manager` interface

### State ownership

| State | Location | Source of truth | Synchronization |
|-------|----------|-----------------|-----------------|
| Started managers | `runtime.managers` (`[]Manager`) | In-memory | `runtime.mu` — `status` reads them from the control socket while the daemon runs |
| Why a manager stayed down | `runtime.startErrs` (`map[id]error`) | In-memory | `runtime.mu` |

### Concurrency model

`runtime.mu` protects `managers` and `startErrs`; `Start` publishes a complete new snapshot, while control-socket status reads may run concurrently. The package owns no goroutine: `Runtime.Start`/`Stop` run synchronously from `main.go`, and each individual `Manager` owns its own loops.

### Data flow

```
main.go -> managers.NewRuntime(cfg, controller) -> runtime
main.go -> runtime.Start(ctx)
  -> for each enabled cfg.UnifiedConfig.Managers entry:
      -> buildManager(entry) -> switch entry.Driver { "telegram": telegram.New(entry, unifiedCfg, controller) }
      -> mgr.Start(ctx)
      -> on error at either step: record startErrs[entry.ID], log, continue
  -> atomically publish runtime.managers = started and runtime.startErrs = failures
main.go -> (registers runtime.Stop as its named stop closure)
  ... later, at shutdown ...
main.go -> runtime.Stop(ctx) -> stopManagers(runtime.managers) in reverse order, first error wins
```

### Lifecycle

`NewRuntime` is a pure constructor and cannot fail. `Start`/`Stop` are called exactly once each by `main.go`, in that order, with `Stop` registered as the runtime's named stop closure at construction time in `main.go` (see [Initialization](#initialization-sequence) / [Shutdown](#shutdown-sequence)).

### Ordering constraints

- **One manager failure does not stop its neighbours**: every enabled entry gets one start attempt. Failures are stored under that entry's own ID and skipped, while successfully started managers remain available so local chat/status can repair bad configuration.
- **`Runtime.Stop` after all managers' own work is safe to interrupt**: each `Manager.Stop` gets the same shutdown `ctx`/budget; `Runtime` itself does not impose a sub-timeout.

### Anti-patterns

- **Don't call `Start`/`Stop` more than once on the same `Runtime`.** `Stop` sets `r.managers = nil`; a second `Stop` would be a no-op stopping nothing, silently masking a bug if `Start` were also called again.
- **Don't add manager-specific logic to this package.** `Runtime` only knows the driver-name string; all behavior lives in the driver package (`managers/telegram`).

### Contracts

- **Unknown driver is a per-manager start error**: `buildManager` returns an error for any `entry.Driver` other than `"telegram"`; `Start` records it under that entry's ID and continues with the remaining managers.
- **A start failure belongs to the manager that failed.** `StartError(id)` answers per manager; a healthy manager's neighbour never inherits its reason. `status` is the only diagnostic a chat has for "your bot token is wrong", so a shared error would send the fix to the wrong place.
- **`RunningIDs` is liveness, not history.** It filters on `Manager.Alive()`, so a manager whose loops exited after a successful `Start` stops being reported as running — and carries no start error, because it did start.
- **Disabled entries are skipped silently**: `entry.Enabled == nil || !*entry.Enabled` entries are never built or started — this is normal configuration, not an error.


<a id="pkg-telegram"></a>

## `internal/managers/telegram`

_Telegram bot manager: session↔topic mapping, commands, voice transcription, rendering. One implementation of `managers.Manager`._


### Purpose

Bridges coagent sessions to a Telegram bot using forum topics — one topic per session, plus a "service" topic for bot-level commands (spawn, kill, etc.). Talks to the daemon exclusively through `controllerapi.Controller` (in-process) and to Telegram through raw Bot API HTTP calls (`m.tg(...)`). Implements `managers.Manager` (`ID`/`Start`/`Stop`).

### Key types

| Name | Exported | Role | Owns |
|---|---|---|---|
| `Manager` | yes (struct) | The Telegram manager instance | `sessionToTopic`/`topicToSession`/`workDirs` maps, `navPaths`/`pathToNav` (folder-picker navigation state), `availableModels`/`availableSkills` caches, `subscription` (`<-chan controllerapi.SessionNotification`), `cancel`/`done` |
| `telegramUpdate`/`telegramMessage`/`telegramCallbackData`/etc. | no (structs) | Telegram Bot API wire types | Nothing — serialization only |

Compile-time check: an inline anonymous interface (`ID()`/`Start()`/`Stop()`) verifies `*Manager` satisfies `managers.Manager` without importing that package just for the assertion.

### File map

- `manager.go` — `Manager` struct, `New(entry, unifiedCfg, controller) (*Manager, error)`, `ID`/`Start`/`Stop`. `Start` sequences: `ensureServiceTopic` -> `reconcileOnStartup` -> `setCommands` -> announce -> `controller.SubscribeAll()` -> spawn `notificationsLoop` + `pollLoop` goroutines (`sync.WaitGroup`, both under one detached `context.WithCancel(context.Background())`)
- `session_map.go` — session↔topic bookkeeping (`registerTopic`/`unregisterTopic`/`getSessionByTopicID`/`getTopicBySessionID`/`setWorkDir`), service-topic persistence (`loadServiceTopicID`/`saveServiceTopicID`), startup reconciliation (`reconcileOnStartup` -- diffs live sessions against known topics), `handleNotification` (routes a `controllerapi.SessionNotification` to topic creation/deletion/messaging)
- `commands.go` — Telegram command/callback dispatch: `handleServiceTopicMessage`, `handleSessionTopicMessage`, `handleHelp`/`handleCommands` (list loaded skills via `controller.ListSkills`, populating `availableSkills`), `handleSchedules` (`/schedules`: read-only listing via `controller.ListSchedules`, formatted by `formatSchedule`/`truncateText` — handled entirely in the manager, never steered into the session so the model can't see it; add/change/remove stay with the agent), `handleSpawn`/`handleSpawnDir`/`handleSpawnFavorites` (folder picker), `handleKill`, `handleModel`, `handleLaunch`/`handleLaunchGWT` (share `displayDir`), `handleCallback` and its sub-handlers. `/new` gets prefix (not exact-match) dispatch here — it carries an argument — and a session-topic redirect; the callback parser (`parseCallbackData`/`cutInt64`/`parseMoreCallback`) grows `newpick:`/`newpage:`
- `commands_new.go` — the `/new` folder-project flow (paired with `commands_new_test.go`): `parseNewCommand` (bare = picker, `<name>` = create), `handleNew`, `createAndLaunchProject` (`controller.CreateProject` → `handleLaunch`, shared by `/new <name>` and a pick), `handleNewPicker`/`buildNewPickerKeyboard`/`relativeAge`/`formatAge` (recency-sorted inline keyboard, callbacks carry the numeric project id — a cyrillic name blows the 64-byte callback limit), `handleCallbackNewPick` (re-lists to resolve id→name, keeping stale keyboards valid) / `handleCallbackNewPage`
- `telegram_api.go` — raw Bot API calls: `tg` (generic POST+decode), `getUpdates` (long-poll), `sendMessage`/`editMessageText`/`deleteMessage`, forum-topic CRUD (`createForumTopic`/`deleteForumTopic`/`editForumTopic`); `sanitizeTransportError` strips the `*url.Error` wrapper (its text embeds the token-bearing request URL) from HTTP errors in `tg` and the voice download, keeping the `%w` chain to the cause
- `render.go` — `textToTelegramHTML` (Markdown-ish to Telegram HTML), `splitMessageChunks` (4000-char chunking), `stripHTMLToPlain`
- `voice.go` — `handleVoiceMessage` -> `transcribeVoice` -> `getTelegramFilePath`/`downloadTelegramFile` -> `transcribeAudio` (OpenAI-compatible transcription API)

### State ownership

| State | Location | Source of truth | Synchronization |
|-------|----------|-----------------|-----------------|
| Session↔topic mapping | `sessionToTopic`/`topicToSession` maps | In-memory, reconciled from `controllerapi.Controller.ListSessions` + session attributes on startup | `Manager.mu` (RWMutex) |
| Working directories per session | `workDirs` map | In-memory | `Manager.mu` |
| Navigation state (folder picker) | `navPaths`/`pathToNav` | In-memory, ephemeral | `Manager.mu` |
| Service topic ID | `serviceTopicID` field + a file under `~/.coagent` (`coagenthome.TelegramServiceFilePattern`) | File is durable source of truth across restarts; field is the in-memory cache | `Manager.mu` for the field; file I/O is synchronous and validated in `loadServiceTopicID`/`saveServiceTopicID` |

### Concurrency model

**Goroutines** (both spawned by `Start`, joined via one `sync.WaitGroup`, sharing one cancellable `context.Background()`-derived ctx):
1. **`notificationsLoop`** — ranges over `m.subscription` (`<-chan controllerapi.SessionNotification` from `controller.SubscribeAll()`), routes each to `handleNotification`.
2. **`pollLoop`** — long-polls Telegram's `getUpdates`, dispatches incoming messages/callbacks to the `commands.go` handlers.

**Mutex**: single `Manager.mu` (RWMutex) guards all the maps above. No IO under lock (map reads/writes only; Telegram/controller calls happen outside the critical section).

### Data flow

#### Session notification -> Telegram message
```
daemon.svc.publish(sessionID, sessionevent.Notification{...}) -> PubSub.Publish
  -> m.subscription channel (from controller.SubscribeAll())
  -> notificationsLoop -> handleNotification(ctx, sn)
      -> resolve topic for sn.SessionID (registerTopic on session_created, lookup otherwise)
      -> render (textToTelegramHTML) -> sendMessage/editMessageText to the topic
```

#### Telegram command -> daemon action
```
pollLoop -> getUpdates -> telegramUpdate{Message or CallbackQuery}
  -> handleServiceTopicMessage / handleSessionTopicMessage / handleCallback*
      -> controllerapi.Controller.{CreateSession,KillSession,SetSessionModel,...}
```

### Lifecycle

1. `New(entry, unifiedCfg, controller)` — pure construction, empty maps, `controller` required (errors if nil)
2. `Start(ctx)` — `ensureServiceTopic` (creates/reuses the service forum topic) -> `reconcileOnStartup` (rebuilds `sessionToTopic` from live sessions + topic attributes, closing topics for sessions that no longer exist) -> `setCommands` (registers the bot's slash-command menu) -> announce message -> subscribe -> spawn the two loops
3. `Stop(ctx)` — cancels the shared ctx, unsubscribes from the controller, waits on `done` (bounded by `ctx`)

### Ordering constraints

- **`ensureServiceTopic` before `reconcileOnStartup`**: reconciliation needs a valid service topic to report orphaned/errored sessions into.
- **`controller.SubscribeAll()` before spawning `notificationsLoop`**: the channel must exist before the loop that ranges over it starts.
- **Cancel before `UnsubscribeAll`/wait-on-done (`Stop`)**: cancelling first lets both loops observe ctx cancellation and exit; unsubscribing after ensures no goroutine is still expecting to read from a channel that's about to stop receiving values.
- **Persist a session-topic binding before caching it**: Telegram must create the remote topic to learn its ID, then persist that ID through `SetSessionAttributes`, and only then update `sessionToTopic`/`topicToSession`. If persistence fails, it deletes the newly created remote topic as compensation. A clear/remap follows the same durable-first order and updates both maps in one mutex section.
- **Persist the service topic before publishing it as manager state**: a missing service-topic file permits one remote create; the returned ID is written through temp file + `Sync` + atomic rename before `ensureServiceTopic` returns it. A write failure deletes the unbound remote topic. Read/JSON/invalid-ID errors are fatal instead of being treated as “missing”, because creating a replacement when durable state is merely unreadable would duplicate the service topic.

### Anti-patterns

- **Don't call any Telegram API method without going through `m.tg`.** It centralizes error decoding (`tgAPIError`) and the shared `httpClient` timeout (45s).
- **Don't mutate the topic maps without `Manager.mu`.** Both loops (`notificationsLoop` and `pollLoop`) touch them concurrently.

### Contracts

- **One topic per session, one service topic for the rest**: `session_created`/`session_cleared` notifications create/remap topics; the service topic never maps to a session ID.
- **Topic attributes are merged, never replaced accidentally**: adding `telegram_topic_id` clones the attributes supplied by the daemon and preserves unrelated keys. An unpersisted topic is never placed in the in-memory routing maps.
- **`reconcileOnStartup` is the source of truth after a restart**: in-memory topic maps are empty on process start; this function rebuilds them from `controllerapi.Controller.ListSessions()` (whose attributes carry the topic ID) before either loop starts, so no notification or command is misrouted during the startup window.
- **Service-topic file moved from `$HOME` into `~/.coagent`, no migration**: old `.coagent-tg-service-<chatID>.json` files at `$HOME` are orphaned; a new file is created under `~/.coagent` on next save.

---

<a id="pkg-ctl"></a>

## `internal/ctl`

### Purpose

The daemon's local control plane: newline-delimited JSON-RPC 2.0 over
`~/.coagent/daemon.sock` at mode 0600. It is pure transport plus one built-in op
(`status`); everything else registers into it.

The constitution's "no inbound listener" rule is about *network* listeners. A
same-user unix socket widens nothing — whoever can open it already runs as the
daemon's user — but the distinction is stated rather than assumed.

### Key types

- `Server` — binds, accepts, dispatches. `Register(op, Handler) error` is how
  other layers add ops without ctl learning what a session or a config is;
  registration closes atomically when the server is marked ready
  (`MarkReady`, which `Serve` does for callers that are ready at construction).
  `ServeStarting` is the daemon's two-phase boot: accept from the bind, answer
  `CodeStarting` until ready.
- `Conn` — one live connection. Handlers receive it so an op can attach a
  server→client push stream (`Notify`) to the connection that asked,
  `AfterReply` schedules work for once the response is on the wire, and `Close`
  ends this connection alone.
- `Client` — a **demultiplexing** read loop. Responses are matched to pending
  calls by id and pushes are fanned to `Notifications()`, because a push landing
  between a request and its response is normal during a chat.
- `Lock` — the single-instance flock at `~/.coagent/daemon.lock`.

### File map

| File | Contents |
|------|----------|
| `protocol.go` | Wire vocabulary: versions, error codes, op names, `Greeting`/`Request`/`Response`/`Notification`, the inbound `frame` |
| `server.go` | Bind, the per-run boot id, per-connection read/write, op registry, `AfterReply`, `Conn.Close` |
| `serve.go` | The accept loop and the starting→ready phase (`Serve`, `ServeStarting`, `MarkReady`) |
| `client.go` | Dial, greeting, `Call`, the demux read loop |
| `handlers.go` | Dispatch and the built-in `status` |
| `types.go` | `Deps`, `StatusResult`, the bootstrap op params, `RestartResult` |
| `lock.go` | Single-instance flock |
| `paths.go` | Socket and lock paths under `~/.coagent` |

### Ordering constraints

- **The flock is taken before the socket is bound and before the database opens.**
  Removing a stale socket file is only safe for the process that has proved no
  other daemon is running.
- The lock fd is close-on-exec (Go's default), so the restart-apply exec drops it
  and the new image re-acquires it. The momentary window is accepted: a racing
  daemon still loses on the flock.
- `NewServer` binds; the composition root calls `ServeStarting` immediately, so
  the socket answers from the moment it exists. It then registers bootstrap ops,
  starts local chat (which registers chat ops), starts configured managers, and
  only then calls `MarkReady`. Until that moment every op — `status` included —
  is refused with `CodeStarting`, so a half-built registry can never surface as
  `unknown method`, and a client dialled during the boot keeps the same
  connection into the ready phase (ADR-0017).
- `Server.Close` closes live connections rather than waiting on them — a chat
  client sits idle on an open socket by design, and the restart drain cannot wait
  for it. It closes each one through `Conn.Close`, so a connection that already
  closed itself is not double-closed.

### Contracts

- **`Conn.Close` ends one connection, and is indistinguishable from the peer
  hanging up.** It is what an owner of a push stream needs: `Notify` is an
  unbounded blocking socket write, so a peer that stopped reading parks the
  pusher until the connection goes away, and before this the only lever was
  taking the whole server down. It is idempotent (a second call is a silent
  no-op), safe from any goroutine, fails an in-flight `Notify` with an error, and
  ends the read loop — which runs the ordinary disconnect cleanup, so the
  connection is removed from the server's set exactly once and `Done` fires once.
  It does not touch `afterReply`: as with a real hangup, a reply that cannot be
  written takes its `AfterReply` hook with it.
- **A rejection is a successful RPC.** JSON-RPC errors are for transport and
  malformed requests only, so a caller can always tell "your input was wrong" from
  "the daemon is gone".
- **Params are never logged.** They carry credentials. Mutating ops log at info by
  method name; reads stay at debug.
- **A bound socket answers.** Connect success is the liveness test, so the phase
  between bind and readiness is reported, not withheld: `CodeStarting` on the
  wire, `ErrStarting` on the client, and a greeting that never arrives within the
  dial budget is classified the same way (an older daemon that binds long before
  it serves). "Starting" is a state a CLI waits on; it is never `ErrNotRunning`
  and never a bare transport error.
- **Handler registration is startup-only.** `Register` returns
  `ErrRegistrationClosed` once the server is ready (and the accept loop rejects a
  second invocation). Empty operations and nil handlers are invalid; `status` is
  reserved for the built-in implementation; duplicate operations are rejected
  without replacing the first handler. These checks are atomic with registration,
  and readiness — not the bind — is the boundary they preserve.
- **`status.boot_id` names the run, not the binary.** A config apply execs the
  same binary into the same pid on the same socket, so version, pid and uptime
  all fail to separate the image that took a request from the one that came back
  for it. The id is random per `NewServer`; clients compare it for equality only.
- **`restart_daemon` is the sudo-free half of an update.** No params, no config
  change, no pending-apply marker: the op name lives here, the handler is
  registered by `cmd/coagent` and does nothing but `AfterReply(applier.Restart)`.
  A daemon too old to know it answers `-32601`, which is the caller's one signal
  to fall back to a full `sudo coagent daemon install`.

---

<a id="pkg-configops"></a>

## `internal/configops`

### Purpose

The one semantic mutation layer for `config.yaml` and the secrets file. Every
facade — the bootstrap socket ops, the session config tools — goes through it, so
guards, `${VAR}` discipline and validation cannot diverge between them.

### The raw-draft discipline

A loaded `config.Config` carries **resolved** credential values in `APIKey` and
`BotToken`. Rendering one back to YAML would write them into `config.yaml` in
plaintext. So `Stage`:

1. loads the file with `config.LoadRawUnifiedConfig` — `${VAR}` preserved,
2. mutates a clone,
3. marshals,
4. proves the bytes load through the strict loader, resolving `${VAR}` against a
   **fresh disk read** of the secrets file (never the boot-time map, so a secret
   written seconds ago already counts),
5. and only then hands back `Staged` for a caller to commit.

`make semgrep` enforces step 1: `config.LoadUnifiedConfig` is banned inside this
package.

### Guards

A mutation may not saw off the branch the daemon sits on. Removing the last
provider is refused; removing a provider any model still references is refused
**naming those models** — no cascade, because silently deleting models the caller
did not name is worse than a refusal it can read; removing `Models[0]` is refused
unless the call names a replacement; a new provider with no key is refused for
drivers whose schema needs one. Guard refusals are `Verdict` rejections, not
restarts into a dead config.

### The write order

`Commit` is backup → marker → config, and the order is the contract. The backup
comes first so the preserved copy is always the one that was live and the marker
can name a file that exists. The marker comes before the write so a crash mid-write
still leaves the next boot something to reason about: its `new_config_sha256`
disambiguates "the write never landed" from "the write landed and something else
broke". `ResolvePending` turns that into one of three outcomes, and the boot that
resolved it must consume it — one left behind arms the next unrelated failure into
a spurious rollback. Both writes fsync the containing directory after the rename,
so the ordering survives power loss and not just a process death.

`ClearPending(p)` is that consumption, and it is identity-checked: it removes the
marker only when the bytes on disk are still the ones `p` was read from. A marker
written by a newer apply is left for its own owner.

### File map

| File | Contents |
|------|----------|
| `configops.go` | `Service`, `Staged`, `Stage`, `Commit`, the raw draft and its clone |
| `ops.go` | `Op` contract, provider and manager ops, the `${VAR}`-only check |
| `ops_model.go` | Model ops and the default-model ordering |
| `verdict.go` | `Verdict`, `FieldError`, `OK`/`Reject` |
| `marker.go` | `Pending`, load/clear/rollback, config hashing |
| `resolve.go` | `ResolvePending` — the boot-time state machine |
| `write.go` | Backup, retention (20), atomic write |
| `secrets.go` | Secrets-file line editing, value encoding, redaction registration |

### Contracts

- **Secrets first, config second.** A crash between the two leaves an orphan
  credential nothing references — harmless. The reverse order would leave a config
  pointing at a `${VAR}` that does not exist, which is fatal at the next boot.
- **The secrets file is edited line by line**, so hand-added comments and unrelated
  entries survive. It is the one file this package never backs up: a backup of a
  secrets file is a second copy of every credential. A write leaves **exactly one**
  assignment of the name: the reader resolves duplicates last-wins, so rotating
  only the first line would leave the stale credential in force. Every form the
  reader accepts counts as an assignment — leading space, `export `, space around
  the separator, and `:` as well as `=`.
- A credential is registered for log redaction at the moment it is written, not at
  the next boot.

---

<a id="pkg-managercli"></a>

## `internal/managers/cli`

### Purpose

The built-in local chat — a peer of the Telegram manager that drives the daemon's
narrow `controllerapi.ChatController` capability over the control socket instead
of receiving the rich Telegram control surface.

It is **not a config entry**, and it is started outside `managers.Runtime`'s
config-driven loop. That is the whole point: it is how a daemon with no config
gets one.

### Behaviour

- On `Start` it get-or-creates the reserved project `coagent` (the existing
  idempotent `CreateProject` path) and subscribes to `SubscribeAll`.
- `chat_open` attaches the connection and resumes the project's most recent live
  session **whatever state it ended in** — a conversation continues; it does not
  restart because the last answer happened to be an error. Zero means the first
  message will create it.
- `chat_send` uses the same durable `SendSessionMessage` path in every runtime
  state. The check-and-create is under one lock, so two terminals racing the
  first message cannot start two sessions; later messages commit to the inbox
  before acknowledgement.
- Sessions are stamped `channel=cli`, which is what gates `request_secret` and the
  onboarding skill.
- Events for that session are pushed to every attached terminal. Concurrent
  terminals are allowed and documented: they see the same conversation and their
  messages interleave.
- **Each terminal owns its socket writes.** `ctl.Conn.Notify` blocks until the
  peer drains, so the single forwarder never calls it: it queues onto a bounded
  per-connection channel that one writer goroutine drains. A terminal that stops
  reading fills its own queue and is dropped from the fan-out with a warning —
  a dead terminal costs one person, a forwarder parked inside a socket write
  costs every terminal and starts the daemon's publish-buffer drops.
- **Dropping a terminal drops its connection.** Every drop path — queue overflow,
  a failed write, the peer disconnecting, `Stop` — goes through one idempotent
  `kill`, which signals the writer *and* calls `ctl.Conn.Close`. Closing the
  socket is the only thing that frees a writer already parked inside `Notify`, so
  the goroutine ends with the drop instead of outliving it until the control
  server happens to close the connection. Nothing is lost by it: a terminal
  reaching `kill` is wedged, already broken, or shutting down.
- Events published before `chat_open`/first-send hands the session id back are
  held in a bounded buffer and released in publication order once the manager
  adopts the id — the daemon publishes for a new session before returning it,
  and dropping that window loses the terminal's own first `idle`.
- A secret request is pushed as its own method, not as chat text — a credential
  must never travel through the chat stream. The terminal keeps the other half of
  that invariant: in `cmd/coagent` the input loop is the only reader of stdin, the
  push reader queues the request instead of prompting, and the loop either enters
  masked mode between reads or answers with the line that was already being typed.
  A line consumed that way is never sent as chat text.
- **A masked prompt outlives the terminal it was pushed to.** The push happens
  once, so `chat_open` re-delivers every request the resumed session is still
  waiting on (`SecretRequests.PendingSecretRequests`, queued onto the attaching
  terminal alone). Without it a reconnect or a second terminal sees its messages
  deferred behind a pending call with nothing on screen explaining it. The daemon
  keeps the truth — the manager holds no prompt state of its own — so two
  terminals prompting at once is fine: the first answer takes the request out of
  the map and the second is refused. `setSecret` claims the request **before** it
  writes, so the refused answer never lands on the variable the winner just set.
- **Resolving a prompt closes it everywhere.** Whoever wins, the daemon publishes
  one `NotifySecretResolved` for that request id (the single-winner `take` is what
  makes it exactly one) and the manager fans it out as `secret_resolved`, ordered
  behind the `secret_request` it closes. Refusing the losing answer is not enough:
  a terminal still parked at the masked prompt would swallow the user's next
  message into a `set_secret` nobody accepts. The winner is told too and
  recognises its own answer coming back, so nothing is announced twice.
- An empty line at the masked prompt declines it: the terminal sends
  `chat_secret_cancel` and the daemon answers the call with the user's refusal,
  so the session stops waiting on somebody who walked away. The prompt text says
  so.
- **The masked prompt is a polled mode of the one reader, not a blocking read.**
  `cmd/coagent` turns terminal echo off for the length of the prompt and keeps
  canonical mode, so the kernel still does line editing and a readable descriptor
  still means one complete line. That is what lets the input loop abandon the
  prompt on a dismissal instead of waiting for a keystroke that is no longer
  wanted — and what it abandons is discarded, terminal buffer and all, because a
  half-typed credential is not chat text.
- Its fake implements exactly `ChatController`; admin/discovery additions to the
  complete controller aggregate cannot break the CLI harness.

---

<a id="pkg-install"></a>

## `internal/install`

### Purpose

Service registration: the systemd unit or launchd plist, the copy of the binary
they point at, and the lifecycle verbs around them. It imports no other internal
package (`coagenthome` aside).

**One install mode per platform, system scope only** ([ADR-0009](docs/adr/0009-system-daemon-user-binary.md)).
Linux writes `/etc/systemd/system/coagent.service` with `User=<login>` and
`WantedBy=multi-user.target`; macOS writes a LaunchDaemon plist under
`/Library/LaunchDaemons` with `UserName=<user>`. There is no user scope: no
`--user`, no user units, no LaunchAgents, no linger handling. `New()` takes no
arguments.

**The binary lives in `<target home>/.local/bin/coagent`**, not in
`/usr/local/bin`. That is what makes updates sudo-free — the user owns the file
the unit points at. It crosses no privilege boundary because `User=`/`UserName=`
drop the daemon off root; a root-executed daemon pointing at a user-writable
binary would be a privesc, and this is not one.

**Root-written paths are handed back.** `sudo coagent daemon install` runs as
root and writes into the target's home, so `installBinary` walks the missing
ancestor segments one at a time (`mkdirAllTracked` — `os.MkdirAll` cannot report
what it created) and chowns them to `target.uid:target.gid`. Directories that
already existed keep their owner. **The directory chown happens before the copy,
not with the binary's** — a failure in between would otherwise leave a root-owned
`~/.local/bin` that the next attempt skips (it exists by then) and the sudo-free
update can never write into. The chown and the euid read go through package `var`
seams (`chownFn`, `geteuid`) so the root path is testable without root. Non-root
installs skip chown entirely.

Two exported entry points serve the update path, both outside the `Manager`
interface because they are not lifecycle verbs:

- `UpdateBinary()` — replace the installed binary with the running one, as the
  plain user. No service manager, no privileges.
- `UnitStale()` — render what this version would write and compare byte-wise with
  the file on disk; a missing file counts as stale. Binary updates never rewrite
  the unit, so a template change sits unapplied until someone reruns the install.
  The caller warns; it never escalates on drift alone. Its filesystem comparison
  lives in `unitFileStale(path, want)`, so tests use a temp path and never infer
  expected state from a developer machine's real `/etc` or `/Library` contents.

`Detect()` is gone — with a single scope there is nothing to probe for.

Unit and plist rendering are covered by golden-file tests, as is the ownership
walk; the verbs shell out to real `systemctl`/`launchctl` and are exercised by
hand.

---

<a id="pkg-version"></a>

## `internal/version`

A single `var Version = "dev"`, stamped at build time from `git describe`. The
fallback is deliberately not a plausible number: a build that forgot the ldflag
must be obvious in a skew report rather than silently claim to be a release. Skew
comparison treats `dev` on either side as incomparable, so onboarding never offers
an "update" that might be a downgrade.
