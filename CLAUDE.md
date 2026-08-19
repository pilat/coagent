# coagent

Coagent is a self-hosted headless coding agent. It runs as a daemon, receives tasks from its built-in Telegram and local-chat managers or from schedules, executes them autonomously via a ReAct loop with tool calling and MCP support, and reports results back. Unlike interactive coding assistants (Claude Code, OpenCode, Roo Code), coagent is designed to work unattended — you send a task and get back the result. The daemon and built-in managers share a private in-process `controllerapi.Controller` contract; coagent does not expose a public controller or plugin API. The binary opens no inbound *network* listener, and the only thing it listens on is a same-user unix socket (`~/.coagent/daemon.sock`, mode 0600) that carries `coagent status`, the onboarding bootstrap and the built-in local chat.

Key differentiators:
- **Headless**: no TUI, no IDE extension, no web UI — pure daemon
- **Multi-session**: each task runs in an isolated session with its own LLM client, tool registry, and conversation history
- **Built-in managers**: Telegram and local chat share the daemon's private in-process controller contract
- **Pluggable LLM backends**: Anthropic, Google Gemini, OpenAI-compatible (including local models)
- **Unattended execution**: sessions persist to SQLite, survive crashes, and resume automatically
- **MCP-first**: MCP server connections are pooled at the daemon level and shared across sessions

## Build & Development Commands

```bash
make build          # go build -o coagent ./cmd/coagent
make test           # go test ./...
make lint           # golangci-lint run ./...
make fmt            # golangci-lint fmt
make arch           # go-arch-lint check (.go-arch-lint.yml tier boundaries)
make semgrep        # semgrep invariants (.semgrep/)
make tools          # online bootstrap for modules and pinned development tools
make all            # non-mutating format check + build + lint + arch + semgrep + tests
make verify-offline # run the everyday gate with Go/uv resolution disabled
make ci             # slow local CI: all + integration + E2E + 5m fuzz + race + stress + mutation

# Run a single test
go test ./internal/session/ -run TestLoopDetect -v

# Run native Linux sandbox integration (requires a container runtime)
COAGENT_TESTCONTAINERS_INTEGRATION=1 mise exec -- go test -count=1 -v -timeout 20m ./internal/bashsandbox -run '^TestLinuxSandboxInTestcontainer$'
```

Go version is managed via `mise.toml` (currently Go 1.25.6).

## Commit Messages

Every commit must follow [Conventional Commits 1.0](https://www.conventionalcommits.org/):
`<type>[optional scope][!]: <description>`. Use the smallest accurate type
(`feat`, `fix`, `docs`, `refactor`, `test`, `build`, `ci`, `perf`, `chore`, or
`revert`), write the description in imperative lowercase without a trailing
period, and mark breaking changes with `!` plus a `BREAKING CHANGE:` footer.

**Never pipe a gate.** `make all 2>&1 | tail` reports the *pipe's* exit status, so a
failing lint/semgrep/test stage reads as success. Run gates bare, or with
`set -o pipefail`, and on any backgrounded run read the output for `make: ***`
before claiming the gate passed. A green exit you did not verify is not a green gate.

## Testing Strategy

Read **[docs/testing.md](docs/testing.md)** before designing tests for any change
involving lifecycle, durable state, queues/ledgers, retries/restart, concurrency,
asynchronous tools or subagents, cross-package event flow, or controller-visible
output. These are temporal protocols: unit tests alone are not sufficient.

The required additional levels are:

- **model-based protocol tests** for ordering, duplicate/stale delivery, races,
  recovery, and state-machine invariants;
- **scenario integration tests** for real daemon → SQLite → session/PubSub →
  manager behavior and the final conversation visible to a user.

Every bug reported from a real conversation must first become a deterministic
scenario fixture preserving the event order and exact observable symptom. Prompt
compliance is never a correctness boundary; scripted models must be allowed to
behave adversarially. Use “E2E” only for compiled-process tests.

### Local CI

GitHub pull requests and main pushes run the shorter Linux/macOS verification;
scheduled/manual jobs run `make ci`. The canonical pre-merge quality gate remains
default-budget `make ci`, locally or in that deep hosted job. It is deliberately
much slower than `make all` and runs sequentially even under `make -j`, because
mutation testing temporarily rewrites production files.

`make ci` includes:

- the complete everyday `make all` gate;
- build-tagged integration with real local programs and hermetic fixtures;
- compiled daemon/control-socket harness E2E three times;
- five minutes of model-based protocol fuzzing;
- the complete default suite under Go's race detector;
- 25 shuffled repetitions of the critical harness/concurrency scenarios;
- scoped mutation testing of tool execution, transcript delivery/reset,
  schedule execution/cleanup, and durable delivery identity, with enforced
  efficacy and mutant-coverage thresholds.

For a quick diagnostic run, budgets may be lowered explicitly, for example:

```bash
make ci CI_FUZZ_TIME=10s CI_STRESS_COUNT=2 CI_E2E_COUNT=1
```

That command is a smoke check, not a successful canonical CI run. Do not report
`make ci` as passed unless the unmodified default budgets completed.

`make check` is the shorter environment-compatibility suite (`make all` plus the
build-tagged integration tests). It may require locally installed `git` and
`gopls`, but it must not use the network or mutable external repositories.

All tests are hermetic with respect to user state. A test that resolves a
coagent-home path must first isolate `HOME` under `t.TempDir()` (or use
`coagenthome.Override` with a temporary directory). Test binaries fail closed on
the inherited home or any path beneath it; semgrep bans direct
`os.UserHomeDir()` calls in tests. Marketplace tests use synthetic names and
temporary local Git repositories only.

## Architecture

### Runtime Flow

```
cmd/coagent/main.go  →  dispatch: bare = onboarding + chat · `daemon` = the daemon
                              ↓
                        daemon (session manager, persistence, private manager contract)
                              ↓                              ↓
                         session (per-task:              managers (Telegram, and the
                          loads CLAUDE.md, skills,         built-in CLI chat over
                          MCP servers, runs ReAct loop)    ~/.coagent/daemon.sock)
                              ↓
                         tools (built-in: bash, read, write, edit, glob, grep, ls, lsp, etc.)
                              + MCP tools (external servers discovered at startup)
                              + skills (SKILL.md loaded from project/global dirs)
```

### Package Layout

The tree under `internal/` is flat — package names say what they provide; dependency tiers are modeled by `.go-arch-lint.yml` + `ARCHITECTURE.md`, not by directory nesting. The only nestings (`tool/builtin`, `managers/telegram`) encode an implements/variant-of relationship.

- **`cmd/coagent`** — CLI entrypoint, wires up daemon (the only importer of `daemon`)
- **`internal/daemon`** — Session manager (`manager.go`), project-identity store (`store.go`), subagent-link ledger (`linkstore.go`), pub/sub events (`pubsub.go`) behind the root-only publish gate (`publish.go`), and the in-process implementation of the `controllerapi.Controller` contract (`controller.go`)
- **`internal/controllerapi`** — The private `Controller` interface + all request/response DTOs, `SessionNotification`, `State*` constants — the leaf contract built-in managers program against so they never import `daemon`; it is not an external extension API
- **`internal/sessionstore`** — Session + message persistence (SQLite): `Store`, `SessionRecord`, `StoredMessage`, atomic compaction replacement/order, and the delivery CAS (`DeliverCompletionAtomic`). A top-level leaf (imports nothing internal) that both `session` and `daemon` depend on
- **`internal/session`** — Per-task session lifecycle and ReAct loop: creates LLM client, loads config/skills/MCP, expands leading `/skill <name>` commands, runs the iterative LLM call → tool execution → observation cycle, manages conversation history via `messageStore`, compacts at a single sanctioned point when the projected request size crosses the threshold, loop detection, tool execution orchestration, checkpoints. Conversation content is never mutated after insert — clearing (compaction's first phase) and compaction are metadata events, and "what the model sees" is a projection computed at load. Sole gating authority (`RegisterGatedTool`)
- **`internal/registry`** — Agent type taxonomy (`AgentType`, `AgentTypeConfig`, built-in types: build/general/explore/compaction) and the immutable per-session `Set` (built-ins + project subagents), prompt templates
- **`internal/llmwire`** — LLM wire vocabulary: `Message`, `Response`, `ToolCall`, `ToolSchema`, `MessageUsage`, role constants. Shared by `session`/`llm`/`tool`
- **`internal/sessionevent`** — Session→controller event vocabulary: `Notification`, `NotificationType`, `Notify*` constants. Shared by `session`/`schedule`/`daemon`/`controllerapi`/`managers/telegram`
- **`internal/llm`** — The private `driverProtocol` interface + registry (`driver.go`): one implementation per provider protocol (`anthropic`, `openai`, `openrouter`, `google-sa`), each owning both client construction and `ListModels`. Client implementations for Anthropic (`anthropic.go`) and OpenAI-compatible (`openai.go`/`openai_http.go`), plus `EnrichCatalog` (`enrich.go`), retry logic and cost tracking. Depends on `config`/`llmwire`/`catalog` — no dependency on `tool`
- **`internal/tool`** — Pure protocol leaf: `Tool`/`Result`/`Registry` contract, `ErrSuspend`, tool-ID constants, no implementations
- **`internal/bashsandbox`** — Native filesystem-write confinement runner shared by Bash and dedicated file mutations: Seatbelt on macOS, Bubblewrap on Linux
- **`internal/shellenv`** — Per-cwd login+interactive shell snapshot (mise/asdf/nvm/direnv toolchain activation) captured once, cached, and replayed via `source` for bash/LSP/MCP subprocess spawns; consumed by `bashsandbox`, `lsp`, `mcp`. Security invariant: captures `os.Environ()` only, never a secrets map
- **`internal/tool/builtin`** — Built-in tool implementations: `bash`, `read`, `write`, `edit`, `glob`, `grep`, `ls`, `lsp_tool`, `apply_patch`, `todoread/todowrite`, `memory_save/delete`, `skill`, `webfetch`, `batch`; also `BuildStack`, the registry+LSP+MCP wiring `session` builds its tool stack from
- **`internal/catalog`** — External model catalogs (models.dev, OpenRouter): fetch with a 5s timeout, disk cache under `~/.coagent/cache/catalog`, parse into `ModelSpec`, and id matching that tolerates dated-vs-undated model ids. A leaf the `llm` drivers build their `ListModels` on
- **`internal/mcpstore`** — MCP server registry (SQLite `mcp_servers`): `project_id NULL` = global, non-NULL = one project; project rows override globals by name. Stores `${VAR}` references literally — resolution happens at acquire time
- **`internal/mcp`** — MCP connection lifecycle with pooling, `AcquireForWorkDir` (takes resolved `ServerConfig` definitions), `Evict` for registry removals
- **`internal/loader`** — Loads CLAUDE.md, SKILL.md, subagent definitions, marketplace plugins from git repos
- **`internal/memory`** — Curated per-project memory (`CuratedStore`): plain SQLite CRUD over the `memories` table, surfaced in the system prompt and managed via `memory_save`/`memory_delete`
- **`internal/schedule`** — Schedule storage, cron validation, executor, plus the `schedule` and `sleep` tools
- **`internal/todo`** — In-memory task tracking (todo lists for the agent)
- **`internal/lsp`** — LSP client for code intelligence tools
- **`internal/managers`** / **`internal/managers/telegram`** / **`internal/managers/cli`** — Manager runtime + the Telegram bot manager and the built-in local chat, all talking to the daemon via `controllerapi.Controller`. A manager that fails to start is recorded against its own id (`RunningIDs`/`StartError`) and skipped, not fatal — the chat is how a bad token gets fixed, and it needs the daemon alive
- **`internal/ctl`** — The control socket: newline-delimited JSON-RPC 2.0 over `~/.coagent/daemon.sock` (0600), an op registry other layers register into, server→client pushes on the same connection (so the client is a demultiplexing read loop), the single-instance flock, and the built-in `status` op. It accepts from the bind and answers every op with a "starting" code until the daemon marks itself ready ([ADR-0017](docs/adr/0017-a-bound-control-socket-answers.md))
- **`internal/configops`** — Every semantic config mutation: the raw (unresolved) draft, the guards, `Verdict`, backup + atomic write + retention, the secrets-file line editor, and the pending-apply marker with its boot-time resolution
- **`internal/install`** — Service registration (systemd unit / launchd plist, binary placement into `~/.local/bin`, ownership handover after a root install, lifecycle verbs, the unit-drift check); imports nothing else internal but `coagenthome`
- **`internal/version`** — The build-stamped binary version
- **`internal/config`** — Environment-based config (`github.com/caarlos0/env/v11`), unified YAML config, marketplace config. Leaf — its only internal import is `coagenthome`; never `logger` (main logs config-load status)
- **`internal/coagenthome`** — Single owner of coagent-home resolution: `UserHome()`/`Dir()`/`Join()` (test-overridable via `Override`) plus a constant for every file/dir name under `~/.coagent`. `os.UserHomeDir` is semgrep-banned outside it
- **`internal/logger`** — Structured logging via `go.uber.org/zap`, including the credential-redaction core (`SetRedactedValues`) that scrubs registered secrets from all log output
- **`internal/git`** — Git operations helper
- **`internal/id`** — ID generation utilities

### Key Concepts

- **Daemon mode**: coagent runs as a long-lived process. Built-in managers call the daemon in-process via the private `controllerapi.Controller` contract. The process binds no network socket; its same-user control socket carries only the documented local operations
- **Sessions**: Each task runs in an isolated session with its own LLM client, tool registry, and conversation history. Sessions can spawn subagents
- **Subagents**: Independent agent instances with clean context, restricted tool sets, and separate iteration limits. Spawned via the `task` tool (implemented in `daemon`, registered onto the session's live registry from outside); monitored via `get_subagent_result`/`send_to_subagent`
- **Project subagent definitions degrade, never disable** ([ADR-0014](docs/adr/0014-subagent-definitions-degrade-never-disable.md)): an `.md` in `.claude/agents/` with no `tools:` key inherits the full inventory (an explicit `tools: []` still means none), and a `model:` the catalog cannot resolve is dropped with one `subagent_model_unknown` warning at load instead of failing every spawn
- **The prompt describes the registry the session actually runs with**: the tools/skills/subagents inventories are recomputed once per activation in `svc.run`, after the daemon has registered `task`/`schedule`/`sleep`/config/MCP tools, and never again inside the loop. A section naming a tool the agent-type allowlist removed is worse than no section — `## Available Subagents` is gated on `task` exactly as the skills block is gated on `skill`
- **Onboarding**: bare `coagent` is a deterministic bootstrap (install-or-update the daemon, collect one provider and its key at a masked prompt over the control socket) followed by an AI-led chat served by the built-in CLI manager. The chat lives in the reserved logical project `sys:coagent` (`sys_coagent` on disk), and its full onboarding skill is automatically active instead of depending on model discovery. Everything past the first provider is configured by asking the agent, which has tools for it
- **Service install + sudo-free updates**: one install mode per platform — a *system* unit/plist (`User=`/`UserName=` drops the daemon to the login user) pointing at a user-owned binary in `~/.local/bin/coagent` ([ADR-0009](docs/adr/0009-system-daemon-user-binary.md)). Sudo appears exactly once per machine, at install: every `coagent daemon <verb>` re-execs itself under sudo through the single gate in `runDaemonVerb`, and there is no user-scope mode to fall back to. Updates replace the binary as the plain user and restart via the `restart_daemon` control op — no systemctl, no password. The unit is never rewritten by an update, so the update path warns on drift instead
- **Config tools + restart-apply**: provider/model/manager mutations are daemon-side tools only on the root session whose project has both the reserved `sys:coagent` name and canonical `<projects_root>/sys_coagent` path; Telegram roots, ordinary project roots and subagents never receive them. User project names cannot contain `:`, user project creation cannot claim `sys_coagent`, and an internal marker cannot grant authority to another path. The terminal-only `request_secret` tool additionally requires `channel=cli` ([ADR-0022](docs/adr/0022-reserved-coagent-configuration-project.md)). A tool validates and guards, *stages* the change and returns `ErrSuspend`; the daemon commits it only once the suspend is durable, then drains and `syscall.Exec`s itself. The verdict comes back through a pending-apply marker — which also keeps the suspended call owned across the restart and is cleared only once the verdict is durably delivered, or is known to be undeliverable because the owed session is gone — and a daemon that cannot boot on the new file rolls back to the backup. One apply is in flight per daemon, across every session and the bootstrap socket alike ([ADR-0015](docs/adr/0015-one-apply-in-flight-one-marker-consumption.md)); a second is refused before it can suspend
- **Pending external calls**: a tool call whose outcome comes from outside the loop (sleep, subagent, a config apply across a restart, a person at a terminal). Never re-executed, never stubbed by transcript repair, and a user message arriving mid-flight queues behind it rather than being appended
- **Manager-owned controller boundary**: the composition root binds one private Controller capability to each manager ID. It stamps that owner on creation, rejects every session-addressed read or mutation for another owner, and delivers only events with the same durable `manager_id`; ownerless and foreign sessions fail closed. Clear/restart preserve the owner; only the reserved CLI project can claim its unambiguous legacy `channel=cli` sessions, while ownerless Telegram sessions remain invisible ([ADR-0023](docs/adr/0023-manager-owned-session-publication.md))
- **MCP pool**: MCP server *connections* are cached at the daemon level with TTL (30min), shared across sessions. The *set* of servers comes from the DB-backed **MCP registry** (`internal/mcpstore`), managed in-session via `mcp_add`/`mcp_remove`/`mcp_enable`/`mcp_disable`/`mcp_list` (root sessions only). Changes propagate through the per-iteration stack rebuild — they take effect from the next run; `remove`/`disable` also `Evict` the pooled subprocess instead of waiting out the TTL
- **Model catalogs**: model metadata (display name, context window, max output tokens, pricing, reasoning capability) is fetched at startup by each driver's `ListModels` and written onto `config.ModelEntry` by `llm.EnrichCatalog`. The catalog is the only source — a model it cannot resolve is a fatal startup error. A bare `openai` provider (no `catalog:` key) flattens every models.dev section in sorted order, so an id several hosts serve resolves to the alphabetically-first section's metadata and enrichment warns (`catalog_section_ambiguous`); set `catalog: <section>` to pin it
- **Reasoning round-trip**: an assistant message stores the provider's own reasoning payload verbatim (`messages.reasoning_raw`) inside a `{model, payload}` envelope — Anthropic thinking blocks, OpenRouter `reasoning_details`. It is replayed on the next request only when the envelope's model equals the current session model; otherwise dropped
- **Marketplace**: Skills and subagent definitions can be loaded from git repos (configured in `config.yaml` under `marketplaces:`)
- **Skills**: Model discovery and direct user invocation are independent. `disable-model-invocation: true` removes a skill from the available-skills inventory and skill tool; `user-invocable: false` rejects `/skill <name>`. A leading `/skill <name> [args]` expands programmatically before the LLM call. A daemon-selected system skill can be activated directly in the static prompt without becoming model-invocable; onboarding uses this path
- **Append-only context log**: Stored message content is immutable after insert. Context transformations are metadata (`compacted_at`, `cleared_at`) plus appended rows; the rendered message list is a projection computed at load, so the prompt prefix is byte-stable between events (cache-friendly)
- **Compaction is the only automatic pressure response** ([ADR-0013](docs/adr/0013-immutable-history-single-compaction-point.md)): no continuous pruning, no separate clearing stage, no decisions keyed on prompt-cache state. It runs at exactly one point in the loop (`applyContextEvents`, where nothing is pending), clears older tool bodies as its own first phase, summarizes everything after the header in one call capped at `compactionOutputReserve`, and rebuilds the transcript as header → summary turn → skill reattachments. No verbatim tail survives; the summary turn carries the brief plus a capped excerpt of the last turns and the still-running background work. `/compact` and `compact_context` raise the same flag that point consumes — behind a non-sleep pending call the request waits in the durable inbox
- **Compaction trigger**: the provider's last reported `PromptTokens` plus a `len/4` delta of what was appended since; with no measurement, a plain whole-transcript estimate (`/status` marks that with a tilde). Three consecutive automatic compactions that fail to relieve the pressure silence the automatic path for the rest of the activation
- **Loop detection**: Diversity-based detector catches repetitive tool call patterns and forces the agent to break out
- **Manual composition root**: no DI framework. `cmd/coagent/main.go` constructs every component by hand and records a named stop closure per component; shutdown replays those closures in reverse
- **Filesystem-write sandbox**: optional native direct-write confinement for Bash descendants and the `write`/`edit`/`apply_patch` tools. This is an integrity boundary, not a confidentiality or multi-tenant boundary: Bash and the read tools can read any file available to the daemon user — including `~/.coagent/secrets` itself — and unrestricted network/MCP paths can disclose that data. MCP/LSP processes and network/Unix-socket effects remain outside the policy. Credentials no longer leak through the inherited environment (see Configuration), but the filesystem and egress vectors are open.
- **WebFetch destination policy**: `webfetch` refuses link-local addresses and the known cloud metadata endpoints, checked after DNS resolution and immediately before `connect`, so the original URL, every redirect hop and DNS rebinding are all covered. It is a targeted mitigation, **not** an SSRF boundary: loopback and private ranges stay reachable on purpose (a coding agent needs to reach the service it is developing), and Bash egress is unrestricted and can reach the same metadata addresses anyway. WebFetch ignores `HTTP_PROXY`/`HTTPS_PROXY` — a proxy would connect on the daemon's behalf and make the check decorative.

### Configuration

- **Secrets never enter the process environment.** `~/.coagent/secrets` is parsed into an in-memory `config.Secrets` map (`godotenv.Read`, not `Load`), so tool subprocesses — Bash, MCP servers, LSP — inherit an environment with no credentials in it. There is no CWD `.env` loading: a task repository cannot inject variables into the daemon.
- `${VAR}` references in `config.yaml` resolve **from that file only**, with no fallback to the real environment, and only in the fields allowed to carry a credential: `providers.*.api_key` and `managers[].bot_token`. Everything else stays literal. MCP server env accepts the same references, resolved against the same map at acquire time rather than at config load. Braced form only (`$VAR` is not expanded, so a literal `$` in an inline key survives), and an undefined reference is a fatal config error naming the variable. Adding a new secret-bearing field means editing `resolveSecrets` in `internal/config/secrets.go` — the sink list is deliberately a whitelist.
- Non-secret knobs (`SUBAGENT_MODEL`) are read via `env.ParseWithOptions` from the secrets map overlaid with the real environment, which wins on conflict.
- **Known credentials never reach log output.** Every resolved credential (secrets-file values, provider `api_key`, manager `bot_token`) is registered with the logger at startup (`logger.SetRedactedValues`) and scrubbed to `[REDACTED]` from all zap entries — messages and string/byte-string/error fields. The telegram client additionally strips `*url.Error` wrappers at the source, since their text embeds the token-bearing request URL. This is a backstop, not a license to log secrets; structured (`zap.Any`) fields are not scrubbed.
- Unified YAML config at `~/.coagent/config.yaml` — defines providers/models, managers, marketplaces, and tool policy. Unknown YAML fields are fatal. There is no `mcp:` section: server definitions live in SQLite.
- **A model entry is `[id, provider]` and nothing else.** `name`, `context_window`, `max_tokens` and `pricing` were removed from the schema — they come from the provider's catalog at startup, and an old config carrying them fails loudly. `TimeoutSec` and `openrouter_config` stay (behavior knobs, not metadata). A provider may set `catalog: <models.dev section>` when its driver name does not identify the vendor; `anthropic` defaults to `anthropic`, `google-sa` to `google-vertex`, and a bare `openai` provider searches every section in sorted order.
- Optional native write confinement:

  ```yaml
  tools:
    bash:
      sandbox:
        enabled: true
        writable_paths:
          - ~/.npm
  ```

  The existing `tools.bash.sandbox` key governs Bash and dedicated file mutations. macOS uses the system Seatbelt executable. Linux requires a trusted root-owned `bwrap` binary. Workspace, system temp, and an existing per-user cache directory are writable by default; add tool-specific caches explicitly. Enabling it protects host files outside those roots from direct mutation; it does not protect readable secrets or make untrusted tasks safe.
- Project-level context: `.claude/CLAUDE.md`, `.coagent/CLAUDE.md`, `CLAUDE.md`
- Skills: `.claude/skills/*/SKILL.md` or `.coagent/skills/*/SKILL.md`
- Subagents: `.claude/agents/*.md` or `.coagent/agents/*.md`

## Architecture Documentation

- **[docs/glossary.md](docs/glossary.md)** — the project vocabulary: what each coagent term means and which synonyms to avoid. Read it first; everything else is written in these words.
- **[ARCHITECTURE.md](ARCHITECTURE.md)** — the single, bounded architecture document. Every production package appears exactly once in its grouped package map; only packages that own lifecycle, durable state, concurrency, trust boundaries or cross-package protocols receive a profile. Obey the anti-bloat contract at the top and never turn it into a file/member inventory, API reference, changelog or test plan.
- **After implementing changes**, run `/pilat:arch-sync` to catch drift between the code and this document before committing.
- Dependency tiers, package-map coverage, durable-protocol/trust headings and the architecture line budget are mechanically enforced by `make arch`; project invariants by `make semgrep`. Both are gated in `make all` and the Stop hook (`make post-stop-hook`).

## Decision Records

Whenever you make a significant or hard-to-reverse design decision (a tradeoff someone might question in six months), write an ADR at `docs/adr/<NNNN>-<slug>.md` (next free number). ADRs explain *why*; ARCHITECTURE.md explains *what*.

## Code Style

See [`docs/coding-style.md`](docs/coding-style.md) for the full style guide —
code-level style (part 1) and architecture-level style (part 2). Key points:

- Interface-first design: export interfaces, not structs. `var _ Interface = (*impl)(nil)` compile-time check
- Constructor `New()` returns interface. Implementation struct is private (lowercase, prefer `svc`)
- File order: `const` → `var` → `type` → exported functions → unexported functions
- Flat error handling — no nested `if err != nil { if errors.Is(...)` ladders
- Structured logging via `logger.Named("name")` or `logger.Ctx(ctx)`
- Tests: table-driven with `testify/assert` + `testify/require`
- File size limit: < 300 lines. Function size: < 50 lines

## Database Migrations

Schema changes are managed by [goose](https://github.com/pressly/goose) v3 with SQL migration files in `migrations/`. Legacy Go migrations 1–6 are preserved as no-ops in `internal/migrate/legacy.go`; the full schema baseline is migration 7 (`00007_baseline.sql`).

### Adding a new migration

1. Create a SQL file: `migrations/00NNN_description.sql`
2. Start with `-- +goose Up` header
3. Use `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` for idempotency
4. For complex data transformations that can't be expressed in SQL, add a Go migration in `internal/migrate/` and register it in `legacy.go` (or a new file) via `goose.WithGoMigrations`

### Rules

- **Never modify an existing migration.** Once merged, a migration is immutable. Fix-ups go into a new migration with the next version number.
- **Test with both fresh and existing databases.** `migrate.Run` on a temp dir covers fresh. For existing-DB scenarios, create the prior schema state first, then verify the new migration applies correctly.
