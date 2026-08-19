# coagent — Architecture

## Maintenance contract

This document describes the current system shape and its stable contracts. It is
not an API reference, code atlas, changelog, test plan, or substitute for an
ADR. Keep only information that changes ownership, durability or recovery,
concurrency, trust boundaries, externally visible behaviour, or a cross-package
protocol. Do not add private member inventories, file maps, method counts,
polling intervals, walkthroughs, test mechanics, or duplicated style/lint
rules. Put rationale for hard-to-reverse choices in an ADR and vocabulary in
the glossary. Update a statement in place and remove superseded text; do not
append history. Keep this document below 1,800 lines. Every Go package belongs
once in the package map; only packages with architecture-bearing contracts get
a profile below it.

Read [the glossary](docs/glossary.md) first. Its terms are normative here:
notably daemon, session, manager, controller, pending external call, delivery
identity, and subagent round.

## System shape

Coagent is a self-hosted, headless coding agent. One daemon owns durable state,
session lifecycle, admission control, the MCP connection pool, and the private
manager contract. It accepts no network listener. The only listener is a
same-user Unix control socket, used by the built-in local chat, bootstrap, and
documented local operations.

```
CLI / Telegram manager ── controller contract / control socket ──> daemon
                                                                  │
                SQLite <── session lifecycle <── per-task session │
                                                                  │
          config, loader, tools, MCP pool, schedules, subagents ──┘
```

A manager submits work as a session. A session owns one agent-loop activation:
its model client, tool registry, prompt projection, and conversation handling.
The daemon owns anything that crosses sessions, survives a process restart, or
needs global admission decisions. Built-in managers program against the private
controller contract, never the daemon implementation. Managers and the local
control protocol are product surfaces, not a public plugin API.

The composition root is `cmd/coagent`. It constructs dependencies explicitly,
registers operations before declaring the daemon ready, and stops components in
reverse construction order. Dependency tiers are enforced mechanically by
`.go-arch-lint.yml`; project-wide invariants are enforced by Semgrep. Avoid
creating a reverse edge merely to access a convenient implementation: introduce
or extend a small contract at the owning boundary instead.

## Package map

The map is an ownership index, not a list of exported symbols. Directory nesting
does not imply a tier except where it expresses an implementation variant.

- `cmd/coagent` — composition root, CLI product policy, onboarding and control-operation wiring.
- `internal/bashsandbox` — native direct-write confinement for Bash and file-mutation tools.
- `internal/catalog` — external model metadata acquisition, caching and identifier matching.
- `internal/coagenthome` — sole resolver and name owner for the coagent home directory.
- `internal/config` — typed configuration and secrets resolution policy.
- `internal/configops` — guarded configuration mutations, backups and restart verdict markers.
- `internal/controllerapi` — private daemon-to-manager contract and DTO vocabulary.
- `internal/ctl` — authenticated local control socket, operation registry and client multiplexing.
- `internal/daemon` — global runtime coordinator, persistence integration and controller implementation.
- `internal/git` — Git operations used by repository-facing features.
- `internal/id` — local identity generation utilities.
- `internal/install` — platform service installation and lifecycle integration.
- `internal/llm` — provider protocol drivers, client creation, retries and cost handling.
- `internal/llmwire` — provider-neutral message, response and tool wire vocabulary.
- `internal/loader` — project context, skills, subagent definitions and marketplace loading.
- `internal/logger` — structured logging and registered-secret redaction.
- `internal/lsp` — language-server process client for code-intelligence tools.
- `internal/managers` — manager lifecycle coordinator.
- `internal/managers/cli` — built-in local chat over the control socket.
- `internal/managers/telegram` — Telegram manager implementation.
- `internal/mcp` — external MCP process lifecycle and daemon-level pooled connections.
- `internal/mcpstore` — durable MCP server definitions and scope precedence.
- `internal/memory` — curated per-project long-term memory.
- `internal/migrate` — SQLite opening and schema migration with production backup policy.
- `internal/registry` — immutable per-session agent-type policy and prompt templates.
- `cmd/releasebuilder` — build-time deterministic archive and checksum composition root.
- `internal/schedule` — durable schedules, sleep ownership and scheduled delivery execution.
- `internal/session` — isolated agent loop, tool gating and transcript projection.
- `internal/sessionevent` — session-to-controller notification vocabulary.
- `internal/sessionstore` — durable sessions, messages, inbox and atomic delivery primitives.
- `internal/shellenv` — captured per-worktree shell environment for child processes.
- `internal/todo` — session-local task tracking.
- `internal/tool` — implementation-free tool protocol, registry and suspension sentinel.
- `internal/tool/builtin` — built-in tools and their common stack construction.
- `internal/version` — build-stamped version vocabulary.
- `migrations` — immutable SQLite schema migration assets.

## Ownership and dependency boundaries

### Durable state

SQLite is the source of truth for runtime facts that must survive restart:
projects, sessions, append-only messages, durable inbox entries, delivery
identity, subagent links, schedules, curated memory, MCP definitions, and
delivery records. Configuration files, their recoverable backups and the
pending-apply marker are atomic filesystem state owned by config operations.
Other memory is coordination or cache only and must be reconstructible from
durable state. A runtime state label shown by a controller is an in-memory
projection; persisted session status is the recovery source.

Session-store operations own transactional message ordering and compare-and-swap
delivery. The daemon owns orchestration across those facts: it must not recreate
them from transcript text or infer correctness from a notification. Schedule,
MCP, config-apply, and subagent packages each own their producer ledger; the
daemon joins those ledgers into a session's runnable and waiting projections.

### Runtime isolation and admission

Each session has separate LLM client, tool registry, history and iteration
budget. A subagent is another session with an independent context and restricted
policy, not a goroutine inside its parent. The daemon enforces total, child,
per-parent and depth limits, retaining overflow in FIFO order. A suspended
parent does not retain an execution slot; its durable pending work does.

The session is the only authority that registers a gated tool. The daemon may
attach control-plane tools to a live session registry, but it cannot bypass
agent-type filtering. Prompt inventories are computed after this registration,
once per activation, so the prompt describes the registry that actually runs.

### Interfaces and extension boundaries

`controllerapi`, `llmwire`, `sessionevent`, and `tool` are vocabulary/contract
packages. They deliberately avoid implementation ownership. The controller
interface is private to built-in managers. MCP servers are external processes;
marketplace content is loadable instruction/definition data, not executable
code with privileged integration rights. New managers and drivers are source
contributions governed by the internal dependency graph.

Agent-type policy is immutable after construction. Registry inputs and returned
configs are copied so a caller cannot mutate global capability policy or another
session's policy. Model metadata belongs to catalogs and drivers; CLI model
recommendations are composition-root product policy rather than catalog data.

## Runtime lifecycle

### Startup and readiness

The executable resolves configuration and coagent-home paths, opens and
migrates SQLite, builds durable stores and daemon services, starts pooled
resources and managers, then marks the daemon ready. The control socket is
bound early so local callers can distinguish starting from absent, but it answers
startup status until every operation owner is registered. A manager startup
failure is isolated to that manager: the daemon and local chat remain available
to repair configuration.

Catalog enrichment occurs at startup. Configured models are validated against
their provider's catalog, so unknown model metadata fails startup rather than
creating a partially specified session. Catalogs are cached for availability but
do not turn configuration into a mutable policy source.

### Session activation

The daemon creates or resumes a session, loads its project context and policy,
constructs the permitted tool stack, then hands control to the session loop. At
each boundary the daemon promotes durable input in FIFO order. It does not append
a user message directly into a live transcript while the session has unresolved
external work. Completion, scheduling and user input use durable paths before a
runner observes them.

The loop asks the LLM, executes returned tools, records observations, and repeats
until a final response, stop, error, suspension, or iteration limit. Retried
provider requests are local to the client; durable operations must be idempotent
across a process or producer retry. Loop detection terminates repetitive tool
patterns rather than treating repeated calls as progress.

### Shutdown and restart

Shutdown stops admission, drains or checkpoints work according to its durable
state, stops managers and pooled resources, then closes stores. Startup recovery
rebuilds runnable sessions from persisted rows and producer ledgers. A stopped
link is retained for explicit follow-up but is not automatically resumed.

Configuration application is a controlled restart: the tool stages work and
suspends; the daemon makes the suspension durable, commits guarded files, then
execs. The next boot resolves the pending marker and injects one verdict before
the owed call can be considered complete. This prevents a restart from losing
the answer or applying two unrelated changes to the same suspended call.

## Durable protocols

### Append-only transcript and compaction

Stored message content never changes after insertion. The model-visible
conversation is a projection of rows plus context metadata, which keeps an
unchanged prompt prefix byte-stable between context events. Oversized tool output
is capped before insertion; the system never retroactively rewrites history.

Compaction is the sole automatic response to context pressure. At one safe loop
point, when no tool call is pending, it clears eligible tool-result bodies as
metadata, summarizes the post-header conversation, appends the replacement turn,
and reattaches required skill context. There is no continuous pruning ladder and
no verbatim tail guarantee. Manual compaction requests raise the same event; a
request behind non-sleep external work waits in the durable inbox.

The trigger combines the provider's last reported prompt tokens with an estimate
of appended content; absent measurement is explicitly approximate. Repeated
automatic attempts that cannot relieve pressure disable only the automatic path
for that activation. The transcript remains the durable audit and recovery
source even when its model projection is compacted.

### Pending external calls: producer ledger and exact result

`ErrSuspend` means a tool has created work whose answer comes from outside the
agent loop. Sleep, blocking subagent work, config apply and terminal input are
examples. A pending external call has a specific tool-call identifier, producer
owner and eventual result. It is never executed again, synthesized by transcript
repair, or bypassed by later normal input.

The owning producer records enough durable state to recover after restart. The
daemon derives pending calls from those ledgers and accepts a result only if it
matches the still-pending call and tool identity. The result enters the durable
inbox, then the append-only transcript at an activation boundary. This exactness
is what prevents stale timers, child completions or restart verdicts from
answering a newer call.

### Subagent creation and completion

The daemon owns the parent-child link ledger and admission decision; the child
session owns its own loop and transcript. Link creation records the parent,
child, activation sequence, delivery obligation and foreground/background mode
before an outcome can be delivered. The `task` tool is registered by the daemon
onto the parent registry, but the parent session remains the gating authority.

Foreground work suspends the parent and owes one result to the parent call.
Background work reports independently without blocking the parent. A child can
have serialized rounds: an activation sequence rejects delayed completions from
an earlier round. Completion delivery uses a transactional compare-and-swap with
the link state, so producer retries are at-least-once but parent transcript
delivery is at-most-once. Cascade stop, failed delivery and restart recovery
preserve the link's obligation until it is resolved or explicitly stopped.

### Schedule delivery identity

Schedules are durable producer records. A one-shot with a tool-call identifier
is a pending sleep and owes an exact suspended-call result; a metadata-free
one-shot is future session input and must not be mistaken for a waiting sleep.
Cron and standalone one-shot deliveries carry a deterministic identity. The
identity fingerprint and transcript mutation commit together, so an identical
retry is acknowledged without another message and a semantic collision fails
closed.

Cron chooses one canonical minute for identity and payload. One-shot delivery
retries are deliberately bounded: ten consecutive same-process failures remove
the row; recreating the executor resets that in-memory counter. This prevents a
permanent retry storm but is not a dead-letter protocol. See ADR-0020 for the
accepted recovery trade-off.

### Configuration restart verdict

Semantic configuration changes belong to config operations rather than direct
tool file edits. Validation works from raw, unresolved configuration. A guarded
commit writes a recoverable backup, records a pending-apply marker, publishes the
new file atomically, then triggers restart only after the caller's suspension is
durable. One daemon accepts one apply at a time.

On boot the marker proves which session and call await an answer, what backup is
available, and which intended configuration hash was written. Successful boot,
rollback, or failure becomes one durable verdict injected to that call. The
marker remains until delivery is durable or the owed session is known gone. This
is a protocol, not a best-effort notification.

### Recovery and root-only publication

The daemon rebuilds recovery state from sessions, durable inbox entries,
schedules, config markers, subagent links and delivery records. It does not
replay arbitrary notifications to reconstitute state. Startup may re-arm
unfinished producer work, but it must validate exact delivery ownership before
making a transcript mutation.

Only root sessions publish session events to managers. Subagent events remain
inside their tree; parent completion is the explicit cross-boundary signal.
Publication is best effort for an individual local control connection: a blocked
push reader must not block RPC replies. The control client exposes a dropped-push
counter; it provides observability, not replay or resynchronization. See
ADR-0021 for this boundary.

## Security and Trust Boundaries

### Credential boundary

Secrets are parsed from the coagent secrets file into an in-memory map, not
loaded into the daemon environment. Child Bash, LSP and MCP processes therefore
do not inherit provider credentials. There is no project-CWD `.env` load.
Braced variable references resolve only from that secrets map and only at
explicit credential sinks; undefined references are fatal. MCP environment
references stay literal in storage and resolve when acquired.

Known resolved credentials are registered for structured-log redaction. This is
a backstop, not permission to log secrets: opaque structured values still require
call-site discipline. Coagent-home resolution has one owner so packages do not
invent inconsistent or test-leaking state locations.

### Filesystem and egress boundary

The optional native sandbox confines direct writes by Bash descendants and
dedicated mutation tools to configured writable roots. On macOS it uses the
platform sandbox; on Linux it requires a trusted root-owned Bubblewrap. It is an
integrity boundary, not a confidentiality, network or multi-tenant boundary:
the daemon user can still read files it can ordinarily read, and MCP/LSP/network
effects remain outside this confinement.

Web fetching rejects link-local and known cloud-metadata destinations after
resolution and immediately before connect, including redirects. It intentionally
allows loopback and private development services and ignores proxy environment
variables, so it is targeted metadata protection rather than a general SSRF
boundary. Bash egress remains unrestricted.

### Local control boundary

The control socket is a mode-0600, same-user Unix socket; it is not a network
API and no inbound network listener is opened. It uses newline-delimited
JSON-RPC with a greeting/readiness distinction. One client read loop demultiplexes
responses and server pushes. A bounded push channel may discard pushes for an
unread consumer so response progress is preserved; durable session state is the
recovery source.

Service installation uses the supported platform service mechanism while running
the daemon as the login user from a user-owned binary. Update/restart flows go
through the control operation, avoiding repeated elevation. Unit drift is
reported rather than silently overwritten.

## Configuration and product surfaces

The unified YAML configuration names providers, models, managers, marketplaces
and tool policy; unknown fields fail closed. Model entries identify a provider
and model ID, while model dimensions and pricing come from the provider catalog.
An operator can pin a catalog section where a protocol driver alone cannot
identify the vendor. MCP server definitions live in SQLite, with project scope
overriding a global entry of the same name.

Project context is discovered from supported CLAUDE/AGENTS and coagent locations;
skills and subagent definitions can come from project, global or marketplace
sources. A missing subagent `tools` declaration inherits the inventory, whereas
an explicit empty list grants none. An unknown subagent model degrades to its
default instead of making every spawn fail. Skill model visibility and direct
user invocation are independent controls; a leading `/skill` command is
expanded before model invocation.

Bare invocation performs deterministic bootstrap, including the initial provider
credential collection over the control socket, and then hands further setup to
the local chat. Telegram is optional: a bad manager configuration must not
prevent the chat used to repair it.

## Package profiles

### Daemon, sessions and persistence

The daemon, session and session-store boundary divides global coordination,
per-task execution and SQLite transaction ownership. The daemon creates runners,
maintains admission queues and event fan-out, owns project identity and subagent
links, and implements the manager controller. It must keep transient maps
reconstructible and defer to stores for durable ordering/CAS decisions.

The session package owns prompt construction, model-tool iteration, context
projection, loop detection and the sole tool-gating API. It receives a prepared
tool stack rather than reaching into daemon state. Session-store owns immutable
messages, compaction metadata/replacement ordering, durable inbox sequencing and
atomic completion delivery. Neither session nor manager may recreate a delivery
by parsing message content.

### Tools and agent policy

The tool package is a pure protocol leaf. It defines the tool registry and the
suspension sentinel without depending on tool implementations, LLM drivers or
the daemon. Built-in tools build a stack from session-scoped dependencies and
delegate direct mutations through the optional sandbox. A suspending tool may
not be batched with ordinary synchronous tools because its result is delivered
after the loop exits.

Registry produces an immutable per-session agent-type set: built-ins plus
project-local overlays. Agent type controls tool filtering, prompt and iteration
policy, while the live session registry controls what is actually callable.
Todo tracking is intentionally in-memory and session-local; it is planning aid,
not durable workflow state.

### LLM, catalog and loading

LLM drivers own provider-specific request/response encoding, client creation,
retry rules and model listing. The wire package carries neutral message and tool
types so session/tool code does not depend on provider SDKs. Reasoning payloads
are stored verbatim with their originating model and are replayed only to that
same model; a model change drops them rather than sending incompatible data.

Catalog owns externally fetched model metadata and cache validity, not product
recommendation. Loader owns trusted local discovery and marketplace retrieval of
instructions and subagent definitions. Loaded content influences a session
prompt and policy input; it never gains an implicit controller or daemon API.
Shell environment capture is per working directory and replayed for Bash, LSP
and MCP subprocesses without merging the secrets map.

### MCP, schedules and memory

MCP-store owns durable definitions; a project row overrides a global row by
name, including a disabled row. MCP owns process acquisition and pooled lifecycle
at daemon scope. Removal or disablement evicts the relevant pooled connection;
new session iterations rebuild their stack so no configuration change takes effect
mid tool call.

Schedule owns cron validation, durable schedule records and execution of sleep
and schedule tools. It depends on a narrow sender contract, not the daemon
implementation. Curated memory is distinct from conversation history, scoped to
a project, and surfaced through the system prompt only as deliberate retained
knowledge.

### Configuration, migration and host lifecycle

Config owns parsing and secret-sink resolution; config operations own semantic
mutation, backup retention and pending-apply recovery. Migration owns SQLite
opening and schema progression. A production upgrade of an existing database
creates a consistent SQLite backup before applying pending migrations and fails
closed if inspection, backup or publication fails; fixture migration runs do not
surprise callers with a backup side effect. Migration assets are append-only.

Coagent-home owns home-path semantics. Install owns service registration and
platform lifecycle only; it does not become a second runtime coordinator. Logger
owns redaction mechanics, version owns build identity, and git/id are local
helpers with no durable protocol ownership.

### Managers, control and releases

The manager coordinator isolates manager failures. CLI and Telegram render
controller state and submit controller requests; they do not directly manipulate
session rows. The CLI is always available with a running daemon, while Telegram
is configuration-dependent. Session-event defines the event vocabulary shared at
this boundary.

Control owns socket framing, readiness, operation registration, single-instance
coordination and push/reply multiplexing. Its best-effort push policy must remain
visible to callers and must not turn an unread client into a daemon-wide stall.
Release builder owns reproducible archive layout and checksum generation; the
release workflow supplies clean tagged source and platform binaries. Official
artifacts target Linux and macOS on amd64 and arm64, include a license and sorted
checksums, and are accompanied by provenance and Sigstore material as described
in ADR-0019. Windows and container artifacts are not an implied support surface.

## Change guidance

When a change crosses a durable producer, restart, queue, timer, subagent,
manager-visible output or concurrent boundary, update the relevant protocol here
and use the terminology in the glossary. Put a significant, hard-to-reverse
trade-off in the next ADR. Changes limited to private implementation detail,
file rearrangement, local algorithm choice or test mechanics normally do not
belong here.

Before merging, run the architecture sync workflow and the architecture checker.
The checker verifies the package map, required durable protocol sections,
security headings and line budget; it complements rather than replaces review of
whether a statement still reflects the code.
