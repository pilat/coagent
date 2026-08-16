# coagent

**A self-hosted, headless coding agent.** You send it a task — from Telegram, the built-in local chat, or a cron schedule — and it works unattended until there's a result: reading and editing code, running shell commands, spawning subagents, picking the work back up after a daemon restart. No TUI, no IDE extension, no web UI. A single Go binary, a SQLite file, zero open ports.

## Why another coding agent

Interactive assistants (Claude Code, OpenCode, Cursor) assume a human at the keyboard, steering. coagent is built for the other mode of work: describe a task, walk away, come back to a result. That one design decision drives everything else:

- **Unattended means durable.** Every conversation turn is persisted to SQLite before the agent reasons over it. Kill the daemon mid-task — sessions, in-flight subagents, and undelivered results are rebuilt from the database on restart and continue where they stopped.
- **Unattended means defensive.** Nobody is watching for an agent stuck in a loop, so a diversity-based loop detector escalates from warnings to blocking to a forced text-only mode. Nobody approves individual file writes, so filesystem confinement is enforced by the OS kernel (Bubblewrap on Linux, Seatbelt on macOS), not by prompt instructions.
- **Unattended means honest about limits.** This README states precisely which security boundaries hold and which don't — see [Security model](#security-model).

Unlike personal AI gateways, coagent keeps its interfaces deliberately small: Telegram and the local chat are built-in managers over an internal daemon contract. It is not a public controller platform and does not expose a third-party plugin or controller API.

## How it works

```
manager (built-in Telegram or local chat)
     │  in-process — no network socket, no HTTP, nothing to expose or patch
     ▼
daemon ─ session manager · admission control · subagent ledger · pub/sub · SQLite
     ▼
session (one per task) ─ ReAct loop: LLM call → tool execution → observation
     ▼
tools ─ 23 built-ins (bash, read/write/edit, apply_patch, grep, LSP, webfetch, …)
        + MCP servers (pooled daemon-wide) + skills + subagents
```

Each task runs in an isolated session with its own conversation history, tool registry, LLM client, and working directory. A session loads project context (`CLAUDE.md` / `AGENTS.md`), skills, and subagent definitions from the target repo, attaches configured MCP servers through a shared connection pool, and loops until it's done, suspended, or told to stop.

## Features

- **Pluggable LLM backends** — Anthropic (native SDK, streaming, explicit prompt-cache breakpoints), any OpenAI-compatible endpoint including local models, OpenRouter (with per-model provider routing), and Google service-account auth. Per-model pricing and context windows, resolved from the provider's catalog, cost tracking that survives context compaction.
- **23 built-in tools** — shell (optionally sandboxed), file read/write/edit plus unified-diff `apply_patch`, glob/grep, `webfetch` (HTML → text), LSP-backed code intelligence (go-to-definition, references, hover — 14 language servers installed lazily on first use), parallel `batch` execution, todo tracking, and persistent per-project memory that's injected into every system prompt.
- **Subagents** — spawn child agents with clean context and restricted tool sets, blocking or in the background. The built-in `explore` type is structurally read-only: its allowlist simply contains no mutating tools, so no amount of prompt injection gives it a write path. Depth is capped at 3, concurrency is bounded by admission control.
- **MCP, pooled** — servers are configured once and shared daemon-wide: connections are deduplicated by a hash of command+args+env+workdir, reference-counted across sessions, and reaped after 30 minutes idle. Tools register dynamically into each session.
- **Skills and marketplaces** — `SKILL.md` skills and subagent definitions load from the project (`.claude/`, `.coagent/`), from `~/.coagent/`, or from git-hosted marketplace repos, with well-defined precedence.
- **Scheduling** — cron sessions (`CRON_TZ` supported) and one-shot wakes. A schedule can run `fresh`: the transcript is wiped on every tick so periodic tasks don't accumulate cruft, while curated memory persists across runs.
- **Telegram manager** — one forum topic per session, a folder picker for spawning sessions (optionally in an isolated git worktree), live model switching, `/status`, voice input via any Whisper-compatible endpoint, and a hard user allow-list.
- **Toolchain-aware subprocesses** — the daemon serves many project directories from one process, each potentially using a different mise/asdf/nvm/direnv setup. A login-shell environment snapshot is captured once per directory (5-minute TTL, stampede-protected) and replayed for every bash, LSP, and MCP spawn, so tools see the same toolchain you'd get in a terminal there.
- **Optional OS-level write sandbox** — see [Security model](#security-model).

## Engineering notes

The parts worth reading the source for.

**Append-only conversation log.** Stored message content is never mutated after insert. Compaction is a metadata event (`cleared_at`, `compacted_at`) plus appended rows; what the model sees is a projection computed at load time. Nothing edits history between compactions, so — combined with deterministically ordered tool schemas — the prompt prefix stays byte-stable for the whole active phase, which is exactly what provider-side prompt caching needs.

**One answer to context pressure.** No continuous pruning, no cache-cooldown heuristics: oversized tool output is capped once, at execution time, and everything else waits for compaction. When the projected request size crosses 85% of the window, compaction replaces the conversation with a written brief — clearing older tool bodies first so the whole dialogue fits one summarization call, a quality gate that rejects summaries missing required sections, and a reattachment budget of 10% of the window so active skill instructions survive. Nothing is kept verbatim, so the brief carries a capped excerpt of the last turns and a list of subagents still running. The trigger reads the provider's own reported token count rather than guessing.

**Crash-safe subagent orchestration.** Every spawn writes a durable ledger row before anything else happens. Completion delivery is a single-transaction compare-and-swap on `delivered_at`: crash recovery and idle-parent revival can both attempt redelivery, and exactly one wins — no duplicated results, no lost ones. On restart, a two-pass sweep rebuilds all in-flight subagent state purely from the ledger; in-memory queues are treated as caches that may diverge and self-heal.

**Deadlock-proof admission control.** A parent blocked on a subagent doesn't wait — it suspends, exits its loop, and releases its run slot entirely. Child slots (12) are capped strictly below total slots (16), so a completing child can always re-admit its now-slotless parent. The classic "parent holds a slot waiting for a child that can't get one" inversion is impossible by construction, not by timeout.

**Suspension as control flow.** Tools like `sleep` and blocking `task` return a sentinel `ErrSuspend`: the session exits cleanly, holds zero resources for however long the wait lasts (minutes or days), and the real tool result is injected on resume — the final transcript reads as if the call had simply returned. Retried spawns are idempotent: an existing ledger row for the same tool-call ID re-suspends instead of forking a duplicate child.

**Loop detection with room to recover.** A sliding window of the last 20 tool calls is scored as `min(unique-args fraction, unique-results fraction)` — catching both "same command over and over" and "same failing result over and over". Low diversity escalates warn → block → (after 3 blocks) forced text-only mode, using Jaccard similarity between windows to tell a persisting loop from a changed strategy. A newly accepted human message resets the detector; so does producing a genuine text response.

**Transcript repair before every call.** A defensive four-op pass runs before each LLM request: drop orphaned tool results, synthesize error stubs for calls that never got a result (but never for legitimately-pending suspended calls), reorder results to follow their calls, drop duplicates. Malformed transcripts fail loudly and locally instead of as cryptic provider API errors.

**Secrets never enter the process environment.** `~/.coagent/secrets` is parsed into an in-memory map — not exported — so bash, MCP servers, and LSP servers inherit an environment with no credentials in it. `${VAR}` references resolve only in three whitelisted config fields (provider API keys, bot tokens, MCP server env), only from that file, and an undefined reference is a fatal startup error naming the variable. There is no working-directory `.env` loading: a task's repository cannot inject variables into the daemon.

**A sandbox that verifies itself.** On Linux, the entire root is remounted read-only with writable roots bound back in; filesystems mounted *underneath* a writable root are discovered via `/proc/self/mountinfo` and re-locked, and the `bwrap` binary itself is verified (root-owned, not writable, not inside a writable root) before being trusted. On macOS, writable paths reach the Seatbelt profile as `-D` parameters — never string-interpolated into policy source. At startup, an enforcement probe writes inside and outside an allowed root and refuses to run if the backend turns out to be a stub.

## Getting started

Requirements: Linux or macOS, Go 1.25+ (`mise install` picks up the pinned toolchain), and an LLM API key.

```bash
git clone https://github.com/pilat/coagent && cd coagent
mise install          # or use your own Go 1.25+
make build            # → ./coagent
./coagent
```

That last line is the whole setup. Bare `coagent` offers to install the service
if none is running, asks for one provider and its key at a masked prompt, and
then drops you into a chat with the daemon. Everything else — more models, a
Telegram bot, MCP servers — you ask the agent for, in that chat. It has tools for
its own configuration, restarts itself to apply a change, and tells you how it
went. It never asks you to type a credential into the conversation: it opens a
masked prompt instead, and the value goes straight to `~/.coagent/secrets`.

```
coagent               set up and chat with the daemon
coagent status        is it running, on what config      (0 running, 2 not, 1 error)
coagent daemon        run it in the foreground
coagent daemon install|uninstall|start|stop|restart
```

If you would rather write the file yourself, it is still just a file. A minimal
working `~/.coagent/config.yaml`:

```yaml
providers:
  anthropic:
    driver: anthropic            # also: openai, openrouter, google-sa
    api_key: ${ANTHROPIC_API_KEY}

models:
  - id: claude-sonnet-5
    provider: anthropic          # limits, pricing and reasoning support come
                                 # from the provider's catalog at startup

managers:
  - id: telegram-main
    driver: telegram
    enabled: true
    bot_token: ${TELEGRAM_TOKEN}
    allowed_user_ids: [123456789]
    target_chat_id: -1001234567890   # a forum-enabled group
```

First run creates `~/.coagent/daemon.db` and applies migrations automatically. Send `/spawn` to the bot, pick a directory, type a task — or send `/new <name>` to spin up a note/chat dialog project without touching the filesystem (bare `/new` picks from recent ones).

Things worth knowing up front:

- **Config parsing is strict.** Unknown YAML keys are a fatal error, on purpose — a misspelled safety setting cannot silently select a zero value.
- **No `config.yaml` is a legal state.** The daemon starts, idles, and serves the local chat — which is how it gets one.
- **`${VAR}` substitution is a whitelist.** It works in `providers.*.api_key` and `managers[].bot_token` — nowhere else in the YAML. (MCP server env accepts the same references, resolved from the same file when the server launches.) Anywhere else the literal string stays.
- **The first model in `models` is the default**; the rest are switchable at runtime (`/model` in Telegram).
- **Model metadata is not yours to write.** A model entry is `id` + `provider`; display name, context window, output limit, pricing and reasoning support are fetched from an external catalog at startup (models.dev, or OpenRouter's own API for the `openrouter` driver) and cached under `~/.coagent/cache/catalog`. A model the catalog does not know cannot be configured — that is the price of having exactly one source of truth. Add `catalog: <section>` to a provider when the driver name alone does not say which vendor is behind the endpoint.

## Configuration

**Write sandbox** (opt-in, Linux needs a root-owned `bwrap`, macOS uses the system Seatbelt):

```yaml
tools:
  bash:
    sandbox:
      enabled: true
      writable_paths:        # workspace, tmp, and the user cache dir are
        - ~/.npm             # writable by default; add tool caches explicitly
```

**MCP servers** live in the database, not this file — the agent manages them from
inside a session with `mcp_add` / `mcp_remove` / `mcp_enable` / `mcp_disable` /
`mcp_list`, scoped `global` (every project) or `project` (this one, and it wins on
a name collision). Env values are stored as `${VAR}` references and resolved from
`~/.coagent/secrets` at launch. Changes take effect from the next run.

**Marketplaces** (skills/subagents from git repos, cached under `~/.coagent/cache`):

```yaml
marketplaces:
  - url: github.com/owner/repo
    plugins: [plugin-name]
```

**Dialog projects** (`/new`). Bare `/new` picks from projects under a root that defaults to `~/.coagent/projects`; override it with:

```yaml
projects_root: ~/dialogs
```

**Project integration.** Context files are concatenated in this order: `~/.coagent/AGENTS.md` → `./AGENTS.md` → `./CLAUDE.md` → `./.claude/CLAUDE.md` → `./CLAUDE.local.md`. Skills load from marketplaces, `~/.coagent/skills`, and the project's `.agents/skills`, `.coagent/skills`, `.claude/commands`, `.claude/skills` (later wins); subagent definitions from `~/.coagent/agents`, `.coagent/agents`, `.claude/agents`. If you already maintain a `CLAUDE.md` and `.claude/` directory for other tools, coagent picks them up as-is.

## Security model

Stated plainly, because an unattended agent deserves precise claims.

What holds:

- **No inbound network surface.** Built-in managers reach the daemon through internal in-process contracts and the Telegram manager is outbound long-polling only. The one thing the process listens on is `~/.coagent/daemon.sock` — a same-user unix socket at mode 0600, carrying `coagent status`, onboarding and the local chat. It widens nothing: whoever can open it already runs as the daemon's user.
- **No credentials in tool subprocesses.** Secrets live in an in-memory map, never in the environment that bash/MCP/LSP inherit.
- **Write integrity (when the sandbox is on).** Bash descendants and the file-mutation tools cannot write outside the workspace and explicitly allowed paths, enforced by kernel primitives with fail-closed startup validation.
- **Structural tool restriction.** A read-only subagent has no mutating tools registered at all — there is nothing for a jailbreak to invoke.

What does not hold — deliberately, and documented rather than papered over:

- **The sandbox is not a confidentiality boundary.** Bash and the read tools can read anything the daemon user can read, including `~/.coagent/secrets` itself. Network egress from bash is unrestricted. MCP and LSP subprocesses run outside the sandbox entirely.
- **`webfetch` protection is a targeted mitigation, not an SSRF boundary.** It blocks link-local and cloud metadata addresses (checked after DNS resolution and before connect, so redirects and DNS rebinding are covered; proxy env vars are ignored so they can't bypass it) — but loopback and private ranges stay reachable on purpose: a coding agent needs to talk to the service it's developing.
- **This is a single-operator system.** There is no multi-tenant isolation of any kind. Do not run tasks for people you don't trust with the daemon user's filesystem.

Treat coagent's autonomy the way you'd treat a new contractor's: real capability, scoped access, and don't hand it the production keys on day one.

## Limitations

- **Two managers ship today.** Telegram, and the built-in local chat over the control socket. Their shared controller contract is internal: another manager is a source-level contribution, not a plugin against a supported external API.
- **Shell-environment capture is bash-only.** zsh/fish users still get a working agent, just without automatic per-directory toolchain activation. Extending to zsh is a known follow-up.
- **Sandbox is Linux/macOS only.** On other platforms `sandbox.enabled: true` fails at startup rather than pretending.
- **Concurrency limits are compile-time constants** (16 concurrent sessions, 12 child agents, subagent depth 3) — sensible defaults, but tuning them means recompiling.
- **Model metadata needs the network on first start.** Catalogs are fetched once at startup and cached on disk; a machine that has never fetched one and is offline fails to start. There is no per-model override to fall back on, by design.
- **LSP servers install themselves on first use.** Existing executables on `PATH` win. Fresh Go/npm/RubyGems installs use pinned versions and atomically published private roots; direct release downloads are pinned per Linux/macOS architecture, SHA-256 verified, and extracted without shell pipelines. It is still an unattended network install that trusts the selected package registries and upstream publishers; `clangd`/OmniSharp are expected on `PATH` regardless.

## Development

```bash
make all         # fmt + build + lint + arch + semgrep + tests
make test        # go test ./...
make lint        # golangci-lint
make arch        # dependency-graph enforcement (go-arch-lint, deepScan)
make semgrep     # project invariants as zero-baseline semgrep rules
make mutation    # mutation testing (gremlins)
```

The codebase is ~29K lines of Go with a ~1:1 test-to-source ratio, including real integration tests: the Linux sandbox is exercised inside testcontainers with an actual `bwrap`, shell snapshots against a live login shell, LSP against real language servers. Architecture rules live in [ARCHITECTURE.md](ARCHITECTURE.md) and are enforced mechanically — the import graph by `go-arch-lint` (with deep scan through interface call sites), coding invariants by semgrep, and SQLite is pure Go (`modernc.org/sqlite`), so builds need no cgo.

## Status

Pre-1.0 and preparing for an open-source release. The storage schema migrates forward automatically; config and internal manager contracts may still change between releases. It runs the author's daily unattended workloads — that's the bar it's been built to, and the bar issues get judged against.

## License

[MIT](LICENSE) © 2026 Vladimir Urushev.
