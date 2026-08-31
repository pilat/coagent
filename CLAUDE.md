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
make ci             # slow local CI: all + integration + E2E + 5m fuzz + race + stress
make mutation       # explicit scoped mutation diagnostic; requires MUTATION_PATH
make mutation.nightly # scheduled-CI shard only; never a commit or PR gate

# Run a single test
go test ./internal/session/ -run TestLoopDetect -v

# Run native Linux sandbox integration (requires a container runtime)
COAGENT_TESTCONTAINERS_INTEGRATION=1 mise exec -- go test -count=1 -v -timeout 20m ./internal/bashsandbox -run '^TestLinuxSandboxInTestcontainer$'
```

The Go toolchain is managed by [`mise.toml`](mise.toml).

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

## Verification Cadence

Tests and static gates are intentionally comprehensive and expensive. Do not run
them after every edit or agent turn.

- During investigation and read-only work, run no verification gates.
- After each coherent behavior or package-level checkpoint, follow the focused
  verification cadence in [`docs/testing.md`](docs/testing.md#development-cadence).
- Run `make all` once after implementation is complete, immediately before the
  final handoff. If it fails, iterate with the specific failing target or focused
  test, then rerun `make all` once after the fix.
- Do not repeat a successful command unless a relevant input changed. Reserve
  complete `make check` and `make ci` runs for an explicit request or the
  corresponding pre-merge gate.

Mutation testing is a diagnostic, never a commit, pull-request, pre-merge, or
final-handoff gate. Do not run `mutation`, `mutation.critical`, or
`mutation.nightly` merely because a change is ready to hand off. Run a focused
`make mutation MUTATION_PATH=./path/to/package` only when the task explicitly
requests mutation evidence or when investigating whether a specific test would
catch a load-bearing guard, retry, deduplication, or error-path defect.

For an explicitly requested large mutation-testing cycle, finish the coherent
implementation batch and run `make all` before collecting mutants. Let that
mutation run finish and collect its complete report before changing tests. Fix
the resulting test gaps as one batch, using only focused target tests while
iterating; then run `make all` once and repeat the same mutation scope once. Do
not interleave one mutant, one fix, and one full-suite run.

Gremlins takes a filesystem path, not a Go package pattern: scoped manual runs
use paths such as `./internal/session`, never `./...`. The nightly target owns
whole-module shard selection. A run that generates zero mutants is not a pass.
Neither is a timed-out mutant: calibrate the timeout from an uncached run of the
slowest affected package, because a cached coverage baseline can make the
derived mutation timeout too short.

Whole-module mutation belongs exclusively to the sharded scheduled or manually
triggered `Nightly Mutation` workflow. It may run for hours and publishes
machine-readable reports. Agents must not run `make mutation.nightly` locally or
add it to `all`, `check`, `ci`, branch protection, or another gate unless the
user explicitly asks to debug that workflow. The nightly job is diagnostic:
survivors are report data, while execution/tooling failures still fail a shard.

## Testing Strategy

Read **[docs/testing.md](docs/testing.md)** before designing tests for any change
involving lifecycle, durable state, queues/ledgers, retries/restart, concurrency,
asynchronous tools or subagents, cross-package event flow, or controller-visible
output. These are temporal protocols: unit tests alone are not sufficient.

## Architecture Documentation

- **[docs/glossary.md](docs/glossary.md)** — the project vocabulary: what each coagent term means and which synonyms to avoid. Read it first; everything else is written in these words.
- **[ARCHITECTURE.md](ARCHITECTURE.md)** — the single, bounded architecture document. Every production package appears exactly once in its grouped package map; only packages that own lifecycle, durable state, concurrency, trust boundaries or cross-package protocols receive a profile. Obey the anti-bloat contract at the top and never turn it into a file/member inventory, API reference, changelog or test plan.
- **After implementing changes**, run `/pilat:arch-sync` to catch drift between the code and this document before committing.
- Dependency tiers, package-map coverage, durable-protocol/trust headings and the architecture line budget are mechanically enforced by `make arch`; project invariants by `make semgrep`. Both are gated in `make all`.

## Decision Records

Whenever you make a significant or hard-to-reverse design decision (a tradeoff someone might question in six months), write an ADR at `docs/adr/<NNNN>-<slug>.md` (next free number). ADRs explain *why*; ARCHITECTURE.md explains *what*.

## Code Style

Follow the full code-level and architecture-level style guide in
[`docs/coding-style.md`](docs/coding-style.md).

## Database Migrations

Follow the [migration rules](docs/coding-style.md#migrations). **Never modify an
existing migration after it has been merged.** Put fixes in a new migration with
the next version number.
