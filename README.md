# coagent

**Hand off a coding task. Close the terminal. Come back to the result — or a
precise blocker.**

coagent is a self-hosted, headless coding agent that runs as a long-lived
daemon. Give it work from Telegram, or let a schedule wake it later. It can
inspect and edit code, run commands
and tests, delegate parts of the work to subagents, and report back when it
finishes or needs a decision.

Accepted input, conversations, schedules, and subagent links are persisted in
SQLite and recovered from durable boundaries after a restart. Telegram uses
outbound polling; local control stays on a same-user Unix socket. There is no
TCP listener, web UI, IDE extension, or public controller API.

> Interactive agents are built for steering. coagent is built for handoff.

coagent is pre-1.0, single-operator software for Linux.

## Quick start

You need Git, Make, [mise](https://mise.jdx.dev/), and credentials for a
supported model provider. mise installs the pinned Go 1.25.6 toolchain:

```bash
git clone https://github.com/pilat/coagent.git
cd coagent
mise install
mise exec -- go mod download
mise exec -- make build
```

There is no first-run wizard: a daemon without a valid model and Telegram
configuration has nothing to talk to. Author `~/.coagent/config.yaml` by hand —
at minimum one provider with a model and one Telegram manager — and keep the
credential values in `~/.coagent/secrets`, referenced from the config as
`${VAR}`. Then install and start the daemon as a system service:

```bash
./coagent install   # sets up and starts the service; auto-escalates via sudo
./coagent status    # exit code 0 means running
```

The binary stays user-owned under `~/.local/bin` and the systemd unit runs it as
your login user with no added capabilities. Beyond Git and a model provider, the
sandbox needs only `bwrap` (Bubblewrap) on the host.

Updating is a manual binary replace plus `coagent restart`.

For Telegram, create a bot with BotFather, send it `/start`, and configure your
numeric user ID in the manager's allowed users. The default target is the private
chat with that sole allowed user. To use a group, add the bot and configure its
numeric `target_chat_id`. Each enabled manager needs its own bot token.

One chat holds the work sessions you select with `/switch N`. With no session
in focus, text and media receive `No session in focus. /new, /spawn or /switch N.`
`/config` in a selected work session authorizes reading the current document
and applying the complete candidate `config.yaml`; an
accepted edit restarts the daemon and rolls back if the configuration cannot
boot. Secrets stay in `~/.coagent/secrets`, maintained by hand outside the
conversation — never paste a credential into a chat.

Send `/spawn /absolute/path/to/checkout` to select a new work session, or
`/new project-name` for a managed project, then give it the task:

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
  run slot while it waits for a timer, a blocking subagent, or a daemon restart.
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
Telegram (outbound polling)          local CLI (`coagent status`)
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

The daemon is the only process-wide owner. It creates isolated sessions, retains
their tool resources, persists lifecycle state, and routes results to the built-in
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
- **MCP:** global and project-scoped servers stored in SQLite. Each session keeps
  its own connections across replies and sleep/resume; policy, configuration or
  shell activation changes replace them. Stop and shutdown close them.
- **Skills and subagents:** project and user `SKILL.md` files, project-defined
  agent types, and opt-in git marketplaces. Tool restrictions are enforced by
  the registered tool set, but Bash remains a powerful escape hatch wherever a
  policy allows it.
- **Schedules:** cron expressions with `CRON_TZ` and one-shot wakes. Fresh
  schedules reset the transcript on every run while curated memory persists.
- **Telegram:** one chat with session switching. Entry shows a header and the
  last three messages, followed by the selected session's live output in order.
  Numbered project/model lists, git worktrees via `/gwt <name>`, status, schedules
  and a hard user allow-list are built in.

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
- **One ordered rule list, last match wins.** On Linux, shell preparation, Bash
  descendants, LSP and stdio MCP processes, and the file tools all run under one
  compiled policy: an ordered list of `allow`/`deny` path rules, evaluated with
  the last matching rule deciding. Coagent ships no rules of its own about the
  operator's machine — every rule names a path only the operator knows. Startup
  fails if the backend cannot enforce the policy.
- **Process output stays readable.** The project section ends with mandatory
  read-only access to its captured output, including for subagents. Denying `~/.coagent`
  still hides other daemon state and does not block inspection of that output.
- **The host is readable, the project is writable, by implicit default.**
  With no configuration at all, the whole host is readable and nothing is
  writable except the session's project — the `projects:` map attaches rules to
  a project, it does not grant it. A read-only `allow` only matters as a
  carve-out from an earlier `deny`.
- **The use without the secret.** The recommended config denies `~/.ssh`, so no
  private key is readable — yet Git over SSH still works: a key is used to
  sign, not to be read, and the sandbox reaches the ssh-agent socket rather
  than the key. Only `known_hosts` and `~/.ssh/config` need re-allowing,
  because host verification and host aliases are not credentials. A bearer
  token has no such split, so denying it disables the tool that needs it.
- **Confined shell preparation.** Shell activation capture and executable lookup
  run inside the session sandbox; each command sources its snapshot through an
  inherited file descriptor.
- **Configuration fails closed.** Unknown YAML keys, missing secret references,
  catalog-unknown models, malformed paths and rules that bury a project's own
  writability are errors rather than silent fallbacks.

What is not enforced:

- Project confinement is an *integrity* boundary, not confidentiality. With no
  rules, an ordinary session reads everything the host filesystem exposes —
  another checkout, the operator's keys, coagent's own secrets file — and
  writes only the project unless the operator's rules say otherwise. Set
  `sandbox.enabled: false` to opt out explicitly.
- **There is no network boundary.** Sessions share the daemon's network: your
  LAN, your host services and your shared database are reachable, and so is the
  whole public internet. The built-in web tools still refuse link-local and
  cloud-metadata addresses, but Bash does not. If you need a network boundary,
  run the whole daemon inside a container or VM you built — that is a deployment
  decision, and coagent does not pretend to make it for you.
- Inherited environment variables and data already inside the project or model
  history are unchanged. The read-only command runtime includes resolver,
  host-name, account, loader, and certificate data needed by system tools.
- Global and marketplace instruction sources remain trusted daemon inputs and
  are read outside the project boundary. Project-local instructions, skills,
  and subagent definitions use the same rooted project access as file tools.
- This is a single-operator system. It provides no multi-tenant isolation and
  should not accept tasks from people you would not trust with the daemon user's
  files.

Treat coagent like a capable contractor: give it scoped access, review its work,
and do not hand it production credentials on day one. See [SECURITY.md](SECURITY.md)
for vulnerability reporting.

## Configuration and project context

Configuration is a hand-edited `~/.coagent/config.yaml`, or a whole-document
`/config` apply from a selected manager-owned work session. The
underlying state is intentionally inspectable:

- `~/.coagent/config.yaml` — providers, models, managers, marketplaces, and tool
  policy; YAML parsing is strict.
- `~/.coagent/secrets` — credential values referenced as `${VAR}` from the
  allowed secret-bearing fields, maintained by hand.
- `~/.coagent/daemon.db` — sessions, messages, schedules, memories, MCP registry,
  and delivery ledgers.
- `~/.coagent/cache` — model catalogs and marketplace checkouts.

MCP servers are managed with `mcp_add`, `mcp_remove`, `mcp_enable`,
`mcp_disable`, and `mcp_list`; they do not live in `config.yaml`.

OpenRouter routing preferences are configured per model. Omitted fields are not
sent, so OpenRouter's account and request defaults remain authoritative:

```yaml
models:
  - id: example/model
    provider: openrouter
    openrouter_config:
      sort: latency # price, throughput, latency, or exacto; mutually exclusive with order
      preferred_max_latency: {p50: 5, p90: 20}
      preferred_min_throughput: 40 # scalar shorthand applies to p50
      allow_fallbacks: true
      require_parameters: true
      data_collection: deny
      zdr: true
      enforce_distillable_text: true
      only: [preferred-provider]
      ignore: [excluded-provider]
      quantizations: [fp8, bf16]
      max_price: {prompt: 1.5, completion: 4}
```

`order` accepts provider slugs in exact priority order as an alternative to
`sort`. Performance thresholds are preferences rather than hard filters.
`max_price` uses USD per million prompt/completion tokens, per request, or per
image/audio unit according to the selected field.

For each repository, coagent understands existing agent conventions instead of
requiring a proprietary project file. It loads `AGENTS.md`, `CLAUDE.md`, skills
from `.agents/`, `.coagent/`, and `.claude/`, and subagent definitions from
`.coagent/agents` and `.claude/agents`. Later, more local sources win when names
collide.

Project confinement:

```yaml
sandbox:
  enabled: true              # omitted means enabled; false is the explicit opt-out
  rules:
    - deny: ~/.ssh
    - allow: ~/.ssh/known_hosts
    - allow: ~/.ssh/config
    - deny: ~/.coagent
    - allow: ~/.cache
      mode: rw
  projects:
    /home/example/projects/service:
      rules:
        - allow: ~/go/pkg/mod
          mode: rw
```

The policy is one ordered list; the last matching rule decides. Coagent ships
no rules about the operator's machine — every path above is something the
operator chose to name. The host is readable and the session's project is
writable by implicit default even with an empty `rules:` list; `projects:`
attaches rules to a project, it does not grant it. A read-only `allow` can reopen
reads after a `deny` or revoke writes from an earlier writable grant. Active
deny targets and read-only exceptions beneath writable grants must exist;
otherwise session startup or process preparation fails explicitly.

See [sandbox-boundary.md](docs/sandbox-boundary.md) for the full model, a
worked example, and a recommended starting block to copy in.

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

- Linux requires Bubblewrap. Enabling the sandbox without it fails loudly.
  A binary built for any other OS refuses to run before it starts.
- The first model-catalog lookup needs network access. Later starts try the
  network again but can fall back to the last valid disk snapshot. Arbitrary
  local model IDs need a matching catalog entry.
- `/spawn <path>` selects an existing checkout in Telegram; `/gwt <name>` from
  a selected work session forks its repository into a fresh worktree branched
  off the remote default branch.
- Shell environment activation is Bash-based. zsh and fish users can still run
  coagent, but do not get automatic per-directory mise/asdf/nvm/direnv capture.
- Language servers are user- or project-owned. Coagent discovers them through
  the project's activated shell PATH. Coagent never downloads or installs them.
- Config, storage schema, and internal manager contracts may still change before
  1.0. Database migrations are automatic and forward-only.

## Command surface

```text
coagent                 print command usage
coagent status          report daemon state
coagent version         print the binary version
coagent install         install and start the service
coagent uninstall|start|stop|restart   manage the service
coagent daemon          run in the foreground
```

Telegram commands operate on the selected session unless they name an ID:

| Command | Effect |
| --- | --- |
| `/new [name]` | List recent managed projects, or create/open one and select a new session |
| `/spawn <path>` | Start and select a session in an existing directory |
| `/sessions` | Show up to 30 live owned sessions, newest activity first; mark the focus and report any overflow |
| `/switch N` | Select a live owned session; repeat to see its last three messages again |
| `/gwt <name>` | Create and select a worktree session from the selected work repository |
| `/kill [N]`, `/stop [N]` | Kill a work session or stop its loop; an ID leaves focus unchanged |
| `/model [N [effort]]` | List configured models or select one by its number |
| `/status`, `/budget <request>`, `/schedules` | Inspect status, authorize a budget change, or list schedules |
| `/clear`, `/compact [focus]` | Replace a work root or compact its context |
| `/config <request>` | Authorize configuration work in the selected work session |
| `/skill <name> [args]` | Activate a skill in the selected session |
| `/help`, `/start` | Show command help |

Text and attachments go to the selected session. Without focus, the manager
replies `No session in focus. /new, /spawn or /switch N.`; voice is not
transcribed and media is not downloaded. Project/session creation, session
listing and switching, numbered kill/stop and help remain available.

Switching sends `→ #N name`, then the last three renderable rows, oldest first,
even when re-selecting the current session. Each row carries its `#N name`
header. Later rows arrive live in order. Tool-calling steps are new messages
with a compact tool trail, including steps with no text. Final answers are new
messages too. Restart preserves the chat cursor; a crash between sending and
saving it can repeat a message. Killing the focus leaves no session selected;
`/clear` selects and enters its successor.

## Development

```bash
make tools          # online bootstrap: modules and pinned development tools
make test           # fast package suites; CI-owned packages excluded
make all            # local format + build + lint + architecture + fast tests
make verify-offline # prove a warmed checkout needs no dependency resolution
make ci             # CI-only: static gates + full ordinary/integration tests
```

Pull requests, main pushes, and releases run `make ci` on Linux, plus
a compiled-harness smoke. Scheduled/manual CI adds full E2E, fuzz, race, and
stress amplifiers. CI-only slow targets require the workflow-provided `CI=true`
environment and reject local execution. Start with
[CONTRIBUTING.md](CONTRIBUTING.md), then see [docs/testing.md](docs/testing.md)
for temporal-protocol test requirements and [ARCHITECTURE.md](ARCHITECTURE.md)
for dependency boundaries.

## Releases

Release artifacts target Linux on amd64 and arm64. Each
release carries checksums, a keyless Sigstore bundle, and GitHub build
provenance. The build rejects a dirty tree and requires the release tag at
`HEAD`; archives are normalized for deterministic output.

<details>
<summary>Verify a release</summary>

```bash
gh release download vX.Y.Z --repo pilat/coagent --dir coagent-release
cd coagent-release
sha256sum --check checksums.txt
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
