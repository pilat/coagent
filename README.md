# coagent

**Hand off a coding task. Close the terminal. Come back to the result — or a
precise blocker.**

coagent is a self-hosted, headless coding agent that runs as a long-lived
daemon. Give it work from Telegram, let a schedule wake it later, or use the
built-in local chat to configure it. It can inspect and edit code, run commands
and tests, delegate parts of the work to subagents, and report back when it
finishes or needs a decision.

Accepted input, conversations, schedules, and subagent links are persisted in
SQLite and recovered from durable boundaries after a restart. Telegram uses
outbound polling; local control stays on a same-user Unix socket. There is no
TCP listener, web UI, IDE extension, or public controller API.

> Interactive agents are built for steering. coagent is built for handoff.

coagent is pre-1.0, single-operator software for Linux and macOS.

## Quick start

You need Git, Make, [mise](https://mise.jdx.dev/), and credentials for a
supported model provider. mise installs the pinned Go 1.25.6 toolchain:

```bash
git clone https://github.com/pilat/coagent.git
cd coagent
mise install
mise exec -- go mod download
mise exec -- make build
./coagent
```

On first launch, coagent offers to install and start its system service. The
initial service installation needs `sudo`; normal chat, configuration, and
binary updates do not. The binary remains user-owned under `~/.local/bin`, while
the systemd unit or launchd plist runs it as your login user.

Next, choose a provider. API-key providers collect the key at a masked prompt
and store it in `~/.coagent/secrets`; Google service-account auth asks for the
JSON file path instead. coagent restarts the daemon and opens the local chat.
From there you can ask it to configure models, a Telegram manager, MCP servers,
and schedules. When a new secret is needed, coagent asks through a masked
terminal prompt instead of sending it through the model conversation.

The local chat owns a persistent dialog project and is primarily the onboarding
and configuration surface. For one person, Telegram setup recommends a private
bot forum: enable Threaded Mode in BotFather, disallow users from creating
topics, send the bot `/start`, and give the local chat your numeric user ID. For
a shared group, use a forum-enabled supergroup and provide its numeric chat ID.
Each manager needs its own bot token; changing a forum target requires a new
manager ID. The local chat requests the token privately. Then send `/spawn` to
the bot, choose an existing checkout, and hand off the task:

```text
Find why TestSettlement occasionally hangs. Reproduce it, fix the root cause,
run the relevant integration tests, and tell me what changed.
```

You can leave after sending it. The daemon owns the run, not your terminal or
Telegram connection.

## Built for handoff

- **Durable work, not a disposable chat.** Accepted input, transcripts,
  schedules, and subagent relationships live in SQLite. After a restart,
  coagent rebuilds work from the last durable boundary instead of opening a new
  conversation.
- **Many tasks, one daemon.** Each session gets its own working directory,
  history, model client, tools, and project context. Admission control bounds
  concurrent sessions and child agents.
- **Waiting is a state, not a worker.** A session can suspend without holding a
  run slot while it waits for a timer, a blocking subagent, a daemon restart, or
  a credential entered at the terminal.
- **Subagents are real sessions.** Children start with clean context and their
  own tool policy. They can run in the background and deliver their result back
  through a durable ledger.
- **Context has one lifecycle.** The transcript is append-only. When it grows too
  large, a single compaction path writes a structured brief and reattaches the
  active instructions instead of continuously rewriting history.
- **Autonomy has brakes.** Loop detection catches repeated calls and repeated
  failures, escalates warnings, and can force the model into a text-only exit.

The detailed invariants and recovery protocols live in
[ARCHITECTURE.md](ARCHITECTURE.md); the reasons behind durable design choices
live in [the ADRs](docs/adr/).

## How it works

```text
Telegram (outbound polling)          local terminal
            │                              │
            │ built-in manager             │ same-user Unix socket
            └──────────────┬───────────────┘
                           ▼
daemon · sessions · schedules · SQLite · subagent ledger
                           ▼
per-task ReAct loop: model → tools → observation
                           ▼
built-in tools · MCP servers · skills · subagents
```

The daemon is the only process-wide owner. It creates isolated sessions, pools
MCP connections, persists lifecycle state, and routes results to the built-in
managers. The managers share a private in-process contract; coagent is an
opinionated product, not an agent framework with a public plugin API.

## What it can use

- **Models:** Anthropic, OpenRouter, Google service-account auth, and
  catalog-resolvable OpenAI-compatible endpoints. Model limits, pricing, and
  reasoning capability come from provider catalogs, so unknown model IDs fail
  at startup instead of running with guessed metadata.
- **Coding tools:** Bash, file operations, unified patches, search, LSP code
  intelligence, web fetch, parallel batches, todos, and persistent per-project
  memory. Language servers are supplied by the project or user toolchain and
  must be available on the activated project `PATH`.
- **MCP:** global and project-scoped servers stored in SQLite. Connections are
  pooled across sessions and reaped after 30 minutes idle.
- **Skills and subagents:** project and user `SKILL.md` files, project-defined
  agent types, and opt-in git marketplaces. Tool restrictions are enforced by
  the registered tool set, but Bash remains a powerful escape hatch wherever a
  policy allows it.
- **Schedules:** cron expressions with `CRON_TZ` and one-shot wakes. Fresh
  schedules reset the transcript on every run while curated memory persists.
- **Telegram:** one forum topic per session, repository picker, git worktrees
  via `/gwt <name>`, model switching, cost/context status, schedules, and a hard
  user allow-list.

## Security model

coagent is an autonomous program running as your user. Its boundaries are
deliberately narrow and explicit.

What is enforced:

- **No TCP listener.** Telegram is outbound-only. Local operations use
  `~/.coagent/daemon.sock`, a same-user Unix socket created with mode `0600`.
- **Coagent-managed credentials are not injected into tool subprocesses.** The
  secrets file is parsed into memory rather than loaded into the environment.
  Ordinary environment variables inherited by the daemon remain visible to its
  children, so do not start it with unrelated credentials exported.
- **Optional write confinement.** On supported Linux and macOS systems, Bash
  descendants and dedicated file-mutation tools can be restricted to the
  workspace and explicit writable paths using Bubblewrap or Seatbelt. Startup
  fails if the enabled backend cannot enforce the policy.
- **Configuration fails closed.** Unknown YAML keys, missing secret references,
  and catalog-unknown models are errors rather than silent fallbacks.

What is not enforced:

- The write sandbox is opt-in and is **not** a confidentiality boundary. Read
  tools and Bash can read anything available to the daemon user, including the
  secrets file. Bash network egress is unrestricted.
- MCP and LSP processes, network effects, and Unix-socket effects are outside
  the write sandbox.
- Web fetch blocks link-local and cloud metadata destinations, but deliberately
  permits loopback and private networks. It is a targeted mitigation, not a
  complete SSRF boundary.
- This is a single-operator system. It provides no multi-tenant isolation and
  should not accept tasks from people you would not trust with the daemon user's
  files.

Treat coagent like a capable contractor: give it scoped access, review its work,
and do not hand it production credentials on day one. See [SECURITY.md](SECURITY.md)
for vulnerability reporting.

## Configuration and project context

The preferred configuration interface is the local chat. The underlying state
is intentionally inspectable:

- `~/.coagent/config.yaml` — providers, models, managers, marketplaces, and tool
  policy; YAML parsing is strict.
- `~/.coagent/secrets` — credential values referenced as `${VAR}` from the
  allowed secret-bearing fields.
- `~/.coagent/daemon.db` — sessions, messages, schedules, memories, MCP registry,
  and delivery ledgers.
- `~/.coagent/cache` — model catalogs and marketplace checkouts.

MCP servers are managed with `mcp_add`, `mcp_remove`, `mcp_enable`,
`mcp_disable`, and `mcp_list`; they do not live in `config.yaml`.

For each repository, coagent understands existing agent conventions instead of
requiring a proprietary project file. It loads `AGENTS.md`, `CLAUDE.md`, skills
from `.agents/`, `.coagent/`, and `.claude/`, and subagent definitions from
`.coagent/agents` and `.claude/agents`. Later, more local sources win when names
collide.

Optional write confinement:

```yaml
tools:
  bash:
    sandbox:
      enabled: true
      writable_paths:
        - ~/.npm
```

The workspace, system temporary directory, and an existing user cache directory
are writable by default. Add language- or package-manager caches explicitly.

### Web search

Sessions have no search by default. Three ways to add it, in precedence order —
an explicit REST provider beats the native default, and an explicit disable
beats everything:

```yaml
tools:
  search:
    provider: tavily            # builtin websearch tool, needs api_key
    api_key: ${TAVILY_API_KEY}  # from ~/.coagent/secrets
    max_results: 5              # 1-10, default 5
```

```yaml
tools:
  search:
    provider: searxng           # builtin websearch tool, self-hosted
    base_url: https://searx.example.com
```

A SearXNG instance must allow JSON output — add `json` to `formats` under the
`search:` section of its settings.yml — or the tool reports the misconfiguration
instead of parsing garbage.

With no `tools.search` section at all, a model on an `openrouter`-driver
provider gets native search by default: OpenRouter executes searches
server-side inside the model turn (billed to your OpenRouter credits, capped at
5 searches per request). Explicitly disabling everything:

```yaml
tools:
  search:
    enabled: false
```

MCP search servers (e.g. Tavily MCP) keep working unchanged. At most one
*integrated* mechanism is active per session — the builtin REST tool or the
native injection — while configured MCP search tools coexist alongside it.

## Know before you run

- Linux and macOS are supported. Enabling the sandbox elsewhere fails loudly.
- The first model-catalog lookup needs network access. Later starts try the
  network again but can fall back to the last valid disk snapshot. Arbitrary
  local model IDs need a matching catalog entry.
- The local terminal chat uses its own persistent dialog project. Repository
  selection lives in Telegram, where `/gwt <name>` inside a session topic forks
  that repository into a fresh worktree branched off its remote default branch.
- Shell environment activation is Bash-based. zsh and fish users can still run
  coagent, but do not get automatic per-directory mise/asdf/nvm/direnv capture.
- Language servers are user- or project-owned. Coagent discovers them through
  the project's activated shell PATH and never downloads or installs them.
- Config, storage schema, and internal manager contracts may still change before
  1.0. Database migrations are automatic and forward-only.

## Command surface

```text
coagent                 set up and chat with the daemon
coagent status          report daemon state (0 running, 2 not running, 1 error)
coagent version         print the binary version
coagent daemon          run in the foreground
coagent daemon install  install and start the service
coagent daemon uninstall|start|stop|restart
```

Inside a Telegram session, `/status`, `/stop`, `/clear`, `/compact`, `/model`,
and `/schedules` are control commands and do not become model instructions.

## Development

```bash
make tools          # online bootstrap: modules and pinned development tools
make test           # fast package suites; CI-owned packages excluded
make all            # local format + build + lint + architecture + fast tests
make verify-offline # prove a warmed checkout needs no dependency resolution
make ci             # CI-only: static gates + full ordinary/integration tests
```

Pull requests, main pushes, and releases run `make ci`; Linux pull requests add
a compiled-harness smoke. Scheduled/manual CI adds full E2E, fuzz, race, and
stress amplifiers. CI-only slow targets require the workflow-provided `CI=true`
environment and reject local execution. Start with
[CONTRIBUTING.md](CONTRIBUTING.md), then see [docs/testing.md](docs/testing.md)
for temporal-protocol test requirements and [ARCHITECTURE.md](ARCHITECTURE.md)
for dependency boundaries.

## Releases

Release artifacts are designed for Linux and macOS on amd64 and arm64. Each
release carries checksums, a keyless Sigstore bundle, and GitHub build
provenance. The build rejects a dirty tree and requires the release tag at
`HEAD`; archives are normalized for deterministic output.

<details>
<summary>Verify a release</summary>

```bash
gh release download vX.Y.Z --repo pilat/coagent --dir coagent-release
cd coagent-release
sha256sum --check checksums.txt             # Linux
# shasum -a 256 -c checksums.txt            # macOS
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/pilat/coagent/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
gh attestation verify coagent_vX.Y.Z_linux-amd64.tar.gz \
  --repo pilat/coagent \
  --signer-workflow pilat/coagent/.github/workflows/release.yml
```

</details>

## License

[MIT](LICENSE) © 2026 Vladimir Urushev.
