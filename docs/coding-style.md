# Coding Style

House style for Go code in coagent, in two parts: **code-level** (how a line is written — naming, error wrapping, testing, file layout) and **architecture-level** (how packages are shaped, wired, and layered). Both are *prescriptive*: they say how new code should look. What the project actually *is* — its package inventory, cross-package contracts, and data lifecycle — lives in [ARCHITECTURE.md](../ARCHITECTURE.md). Rule of thumb: if it shapes how a line reads → code-level; if it shapes imports, ownership, or wiring → architecture-level; if it describes the current system → ARCHITECTURE.md.

Existing violations are debt, not precedent: new code follows this file; touched code moves toward it without repo-wide churn. When a convention here changes, update this file in the same PR, and run `/pilat:arch-sync` to catch drift.

---

## Code-level style

How an individual line or file of Go is written — naming, error handling, logging, testing, and the anti-patterns we reject.

### Project Decisions

These are local project decisions. They apply to new code and to code being
touched during feature work:

- Use `go.uber.org/zap` behind `internal/logger`.
- Use SQLite + raw `database/sql` + goose migrations.
- Use hand-written test doubles; do not add mockery-generated mocks to new
  packages.
- No DI framework. Composition is a hand-written root in `cmd/coagent/main.go`:
  components constructed in dependency order, a named stop closure registered
  per component, replayed in reverse on shutdown.
- Treat existing violations as debt, not precedent. New code follows this file;
  touched code should move closer to it without repository-wide churn.

### Project Structure

```
cmd/<binary>/main.go    # CLI entrypoint, composition root, signal handling
    ▼
internal/               # flat package tree; dependency tiers are modeled by
                        # .go-arch-lint.yml, not by directory nesting
```

**Rules:**

- One package = one responsibility. No `package main` outside `cmd/`.
- Tier mapping and import-direction rules: [§1](#1-layering).
- Leaf utility/protocol packages (`logger`, `llmwire`, `id`, …) must not import from the component tier. There are no global common-component exceptions: every allowed project import is explicit in `.go-arch-lint.yml`.

### Interface-First Design

Package shape (interface at top, private implementation, compile-time check, constructor returns interface) is defined in [§2](#2-package-shape). Canonical code form:

```go
type Service interface {
    Do(ctx context.Context, input Input) (Output, error)
    Close() error
}

var _ Service = (*svc)(nil)

type svc struct {
    dep1 Dep1
    dep2 Dep2
}

func New(dep1 Dep1, dep2 Dep2) Service {
    return &svc{dep1: dep1, dep2: dep2}
}
```

- `var _ Interface = (*impl)(nil)` sits near the implementation, after the
  public interface has been declared.
- `New*()` returns the interface for behavior-bearing types. Concrete-pointer
  returns are only for pure data/helper objects with no substitutable behavior.

### Naming Conventions

| What | Pattern | Example |
|------|---------|---------|
| Interface | `Service`, `Client`, `Registry`, `Store`, `Manager` | `type Service interface` |
| Service implementation (root-wired component) | **`svc`** | `type svc struct` |
| Leaf utility implementation | purpose-named lowercase | `type client struct`, `type parser struct` |
| Constructor | `New` (or `NewFoo` when a package exposes multiple) | `func New() Service` |
| Constants | CamelCase, grouped in a `const (...)` block | `const (KindFunc = ...)` |
| Test files | `*_test.go` next to code | `parser_test.go` |

**Rules:**
- For services wired by the composition root — use `svc` for the implementation struct. Grep-friendly, consistent across packages.
- For leaf utilities — purpose-named lowercase is fine (`client`, `parser`, `resolver`). These packages often have multiple types or none, so `svc` doesn't fit.

### Error Handling

```go
// Wrap errors with context — terse, lowercase, no "failed to" prefix
func (s *svc) ParseFile(content, path string) (*Graph, error) {
    tree, err := s.tree(content)
    if err != nil {
        return nil, fmt.Errorf("parse %s: %w", path, err)
    }

    graph, err := s.buildGraph(tree, path)
    if err != nil {
        return nil, fmt.Errorf("build graph for %s: %w", path, err)
    }

    return graph, nil
}
```

**Rules:**
- Wrap with `fmt.Errorf("<terse lowercase context>: %w", err)`. Include the identifier (file name, ID, action) in the context string.
- **No "failed to X" prefix.** The `%w`-chain already reads as a failure path; "failed to parse: failed to read: failed to open" is noise. Write `"parse: %w"` / `"read: %w"` / `"open: %w"`.
- No trailing punctuation, no capital first letter — standard Go error convention.
- Extra non-wrapped detail goes outside `%w` when the underlying error wouldn't carry it: `fmt.Errorf("git pull %s: %w (output: %s)", url, err, string(out))`.

Sentinel / typed error policy, propagate-vs-swallow rules: [§7](#7-error-architecture).

### Logging

Use `go.uber.org/zap` via a project-local wrapper package (conventionally `internal/logger` or equivalent). The wrapper exposes:

- `logger.L` — global `*zap.Logger` (**forbidden outside the `logger` package itself**)
- `logger.Named(name)` — returns a named child of `L` for background / lifecycle code
- `logger.Ctx(ctx)` — returns the logger stored in ctx, falls back to `L` if none
- `logger.With(ctx, fields...)` — returns a new ctx with enriched logger
- `logger.ToContext(ctx, lg)` — stores a logger in ctx

```go
import (
    "context"

    "go.uber.org/zap"
    "example.com/project/internal/logger"
)

func (s *svc) Do(ctx context.Context, input Input) error {
    log := logger.Ctx(ctx).Named("pkg.do")

    log.Info("starting",
        zap.Int64("id", input.ID),
        zap.String("mode", input.Mode),
    )

    // ...

    log.Debug("iteration",
        zap.Int("n", n),
    )

    return nil
}
```

**Code-level rules:**

- **If you have `ctx` — use `logger.Ctx(ctx)`.** Always. The context may carry enriched logger with correlation IDs, session info, trace spans. Only fall back to `logger.Named(...)` when no ctx is available (constructors, init code).
- Use `zap.Field` constructors (`zap.String`, `zap.Int64`, `zap.Error`, `zap.Any`) — never `fmt.Sprintf` into the message string.
- Levels: `Debug` for trace detail, `Info` for milestones, `Warn` for recoverable issues, `Error` for situations you're both logging AND propagating.
- `zap.Error(err)` preferred over `zap.String("err", err.Error())` — zap renders typed errors better.

Acquisition policy (who uses `Ctx` vs `Named` vs struct-field, dotted-name convention, `logger.L` prohibition): [§8](#8-logging-architecture). Propagate-vs-swallow rules: [§7](#7-error-architecture).

### Testing

This section defines test code style and environment boundaries. The mandatory
rules for choosing **which level of evidence** a change needs — unit,
model-based protocol, scenario integration, or E2E smoke — live in
[testing.md](testing.md). Read that document for lifecycle, persistence,
concurrency, asynchronous, cross-package, or controller-visible changes.

#### Table-Driven Tests

```go
func TestParser_ParseFunction(t *testing.T) {
    tests := []struct {
        name     string
        input    string
        want     *Symbol
        wantErr  bool
    }{
        {
            name:  "simple function",
            input: `package foo; func Bar() {}`,
            want:  &Symbol{Name: "Bar", Kind: KindFunc},
        },
        {
            name:  "method with receiver",
            input: `package foo; func (s *Svc) Bar() {}`,
            want:  &Symbol{Name: "Bar", Kind: KindMethod},
        },
        {
            name:    "invalid syntax",
            input:   `package foo; func {}`,
            wantErr: true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            p, _ := New()
            got, err := p.ParseFile(tt.input, "test.go")

            if tt.wantErr {
                assert.Error(t, err)
                return
            }

            require.NoError(t, err)
            assert.Equal(t, tt.want.Name, got.Symbols[1].Name)
            assert.Equal(t, tt.want.Kind, got.Symbols[1].Kind)
        })
    }
}
```

#### Mocking

**Hand-written mocks, not `mockery` / `testify/mock`.** Avoid
mock-generation tooling. Interfaces in this style are small enough that a
20-line hand-rolled struct satisfying the interface is cheaper than maintaining
generator config.

```go
type mockSender struct {
    mu    sync.Mutex
    calls []sentCall
    err   error
}

func (m *mockSender) Send(ctx context.Context, id int64, payload string) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    m.calls = append(m.calls, sentCall{id, payload})
    return m.err
}

func (m *mockSender) Notify(id int64, event Event) {}
```

Guard concurrent mock access with a mutex when the subject-under-test spawns goroutines.

#### Test Libraries

- `github.com/stretchr/testify/assert` — non-fatal assertions
- `github.com/stretchr/testify/require` — fatal assertions (use for setup invariants)
- Stdlib `testing` only (no testify) is acceptable for simple packages.

Use a real database in tests — open a fresh SQLite file under `t.TempDir()` and run migrations on it. No SQL mocks — schema drift is the primary risk, and only a real DB catches it.

#### Parallelism

- Call `t.Parallel()` in pure-compute unit tests and in tests that only touch `t.TempDir()` / in-memory state. Parallel tests are the default for new files.
- Do NOT parallelize tests that share mutable state outside their `TempDir`: real external subprocesses, fixed ports, global registries, or package-level caches.
- When a subtest uses `t.Run(name, func(t *testing.T) { ... })`, call `t.Parallel()` inside the subtest closure, not at the outer scope, if the subtests are independent.

#### Unit vs integration boundary

- **Unit tests** live next to the code as `<name>_test.go`. Fast (<100ms), no subprocesses, no network, no external tooling on PATH. Run by default under `go test ./...`.
- **Integration tests** live in `<name>_integration_test.go` or a dedicated file with a build tag:

  ```go
  //go:build integration

  package foo
  ```

  Run via `go test -tags=integration ./...`. Tests that require real subprocesses or external tooling on `PATH` go here. Canonical integration targets remain hermetic: use loopback fake servers and temporary local Git repositories, never mutable upstream content.
- Guard environment-dependent integration tests with an explicit skip when prerequisites are missing: `if _, err := exec.LookPath("some-tool"); err != nil { t.Skip("some-tool not installed") }`.
- Credentialed network smoke tests use the `live` build tag and are never part of `make all`, `make check`, or `make ci`. They may read only explicitly supplied environment variables; never load a developer's dotenv file or depend on a personal filesystem path.
- Tests that resolve a coagent-home path must isolate `HOME` under `t.TempDir()` or use `coagenthome.Override` with a temporary directory. Direct `os.UserHomeDir()` is banned in tests as well as production packages.

### Codegen, embedded assets, build tags

- **`//go:embed`** for assets compiled into the binary — migration SQL, default configs, example payloads. Prefer embed over runtime file reads for bundled-with-binary data.
- **`//go:generate`** is reserved for codegen that runs via `make generate` or CI — never a magic incantation the developer must remember. If you add a generator, document the invocation in the `Makefile`.
- **Build tags** on the FIRST line of the file with a trailing blank line before `package`:

  ```go
  //go:build integration

  package foo
  ```

  Use `integration` for hermetic external-program tests and `live` only for credentialed network smoke tests. Avoid inventing further tags without a reason — each tag is a matrix dimension for CI.

### Dependencies

No DI framework — `cmd/coagent/main.go` is a hand-written composition root. Code-level notes:

- Constructor arguments are plain Go values / interfaces — positional, no DI-bag param structs.
- A component that owns background work exposes explicit `Start(ctx) error` / `Stop(ctx) error` (or `Shutdown(timeout)`); the root registers a named stop closure per component and replays them in reverse order on shutdown.
- Leaf packages expose constructors, never package-level wiring helpers.

Full wiring rules (composition-root discipline, lifecycle placement):
[§3, §4](#3-dependency-injection) — read their fx-phrased
rules as "the composition root", per the Architecture-level part intro.

### File Size Limits

| What | Limit |
|------|-------|
| Function | < 50 lines |
| File | < 300 lines |
| Package | < 1000 lines |

These limits are guardrails against unreadable code, not a license to create
single-use helper chains. If a file/function exceeds the limit, split by real
responsibility or document why a flat workflow is clearer. Existing large files
are debt; do not grow them casually.

### Code Organization in File

```go
package resolver

import (
    "context"
    "fmt"

    "example.com/project/internal/logger"
    "example.com/project/internal/types"
)

// Constants
const (
    maxPasses = 10
)

// Public interface
type Service interface {
    Resolve(ctx context.Context, graph *Graph) error
}

// Compile-time check
var _ Service = (*svc)(nil)

// Private types
type svc struct {
    // ...
}

// Constructor
func New() Service {
    return &svc{}
}

// Public methods (interface implementation)
func (s *svc) Resolve(ctx context.Context, graph *Graph) error {
    // ...
}

// Private methods
func (s *svc) resolvePass(graph *Graph) int {
    // ...
}

// Helper functions (package-private)
func isBuiltinType(name string) bool {
    // ...
}
```

### Database

House convention: **SQLite via `modernc.org/sqlite`** (pure-Go, no CGO) through raw `database/sql`. Migrations are run by [goose](https://github.com/pressly/goose) v3 from SQL files embedded via `//go:embed`.

#### Store shape

Each bounded context has its own `Store` interface and implementation in its package. Constructor takes the shared `*sql.DB`:

```go
type Store interface {
    Get(ctx context.Context, id int64) (*Row, error)
    Add(ctx context.Context, r *Row) (int64, error)
    // ...
}

type store struct {
    db *sql.DB
}

func NewStore(db *sql.DB) Store {
    return &store{db: db}
}

func (s *store) Get(ctx context.Context, id int64) (*Row, error) {
    row := s.db.QueryRowContext(ctx, `SELECT ... FROM rows WHERE id = ?`, id)
    return scanRow(row)
}
```

#### Context is always passed

All Store methods take `ctx context.Context`. Use `ExecContext` / `QueryContext` / `QueryRowContext` — never the non-ctx variants. Context lets callers enforce deadlines and cancellation.

#### Migrations

- Files live in `migrations/` as `<number>_<description>.sql` (zero-padded 5-digit number).
- Start with `-- +goose Up`.
- Prefer `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` for idempotency.
- **Never modify an existing migration after it has been merged.** Fix-ups go in a new migration with the next number.

Store ownership, transaction-ownership boundary, and multi-store atomicity rules: [§5](#5-persistence).

### JSON Serialization

Every exported field on a type that crosses a process boundary (wire protocol, RPC, HTTP body, persisted metadata) carries an explicit `json:"..."` tag. Implicit default tags (Go's auto-generated CamelCase keys) are forbidden — they silently couple the wire format to field renames.

```go
type Event struct {
    Type      EventType `json:"type"`
    Message   string    `json:"message,omitempty"`
    ID        int64     `json:"id,omitempty"`
    CreatedAt time.Time `json:"created_at"`
}
```

Where wire types live and how persistence-only fields are separated from wire shapes: [§6.3](#6-cross-package-contracts).

### Quick Checklist

Before committing, verify:

- [ ] `make test` passes (`go test ./...`)
- [ ] `make lint` is clean
- [ ] No `package main` outside `cmd/`
- [ ] Interfaces declared above struct; `var _ Interface = (*impl)(nil)` present
- [ ] `New*()` returns the interface, not `*struct` (see [§2](#2-package-shape))
- [ ] Errors wrapped as `fmt.Errorf("context: %w", err)` with terse lowercase phrasing
- [ ] Table-driven tests for anything non-trivial
- [ ] Files < 300 lines, functions ≤ 50 lines
- [ ] Logger acquired once at function entry, dotted `Named("pkg.file")`, no bare `logger.L` outside the logger package
- [ ] `context.Context` threaded through every IO / DB / LLM call
- [ ] `BeginTx(ctx, nil)` — not plain `Begin()` — for transactions

### Core Principles

#### Fail Fast
- Don't check for nil if the value should never be nil
- Trust your invariants — don't add defensive checks for impossible states
- If something is wrong, panic early rather than corrupt data silently

#### No Arrow Problem (Avoid Deep Nesting)
```go
// BAD: Arrow/ladder anti-pattern
func process(items []Item) error {
    if len(items) > 0 {
        for _, item := range items {
            if item.Valid {
                if item.Type == "special" {
                    // deeply nested logic
                }
            }
        }
    }
    return nil
}

// GOOD: Early returns, flat structure
func process(items []Item) error {
    if len(items) == 0 {
        return nil
    }
    for _, item := range items {
        if !item.Valid {
            continue
        }
        if item.Type != "special" {
            continue
        }
        // logic at same indentation level
    }
    return nil
}
```

#### Minimal Exports
- Never export what doesn't need to be exported
- Start with everything private, export only when required by external packages
- Internal types, helpers, constants — keep them lowercase

#### Comments in English Only
- **All code comments MUST be in English** — no exceptions
- Russian is allowed only in user-facing strings and documentation files (*.md)
- Commit messages in English

### Documentation Style

#### What We Hate
- **Anything above a `package` clause except a build directive.** No `// Package foo does...`,
  no working notes, no "what this file does" preamble — not one line. What a package is for
  belongs in `ARCHITECTURE.md`, which is where a reader actually looks; a doc comment is a
  second copy that silently rots. Mechanically enforced by `make semgrep`
  (`coagent-no-preamble-before-package`), and `revive`'s `package-comments` plus staticcheck's
  `ST1000` are off for the same reason — nothing anywhere asks for these.
- Trivial comments that repeat what code already says (`// increments counter`)
- Comments on every single function

#### What We Love
- Godoc comments on **exported** methods — above the function signature
- Comments that explain **why**, not **what**
- No comments when code is self-explanatory

```go
// BAD: Trivial, useless
// NewParser creates a new parser
func NewParser() *Parser { ... }

// BAD: Package doc noise
// Package parser provides parsing functionality for Go files.
package parser

// GOOD: Explains non-obvious behavior
// ParseFile processes the file in streaming mode to handle large inputs.
// Returns partial results even if parsing fails midway.
func (s *svc) ParseFile(content string) (*Graph, error) { ... }

// GOOD: No comment needed — function name is clear
func (s *svc) Close() error { ... }
```

### Anti-Patterns

**Don't:**

```go
// BAD: Exported struct instead of interface
type Parser struct { ... }
func NewParser() *Parser { ... }

// BAD: Import alias without genuine conflict
import orguc "example.com/project/internal/usecases/org"

// BAD: No error context
return err

// BAD: log.Printf
log.Printf("parsing %s", path)

// BAD: Huge function
func DoEverything() { /* 200 lines */ }

// BAD: Global state
var globalParser *Parser

// GOOD: Singleton for expensive resources (sync.Once ensures thread-safety)
var (
	parsers     map[string]*sitter.Parser
	parsersOnce sync.Once
)

func getParser(lang string) *sitter.Parser {
	parsersOnce.Do(initParsers)
	return parsers[lang]
}

// BAD: Defensive nil check for impossible case
func (s *svc) Process(graph *Graph) {
    if graph == nil {  // graph is NEVER nil here
        return
    }
    // ...
}

// BAD: Arrow problem
if x {
    if y {
        if z {
            doSomething()
        }
    }
}

// BAD: Exporting internal helpers
func HelperThatNoOneNeedsExternally() { ... }

// BAD: Comment in Russian
// Парсим файл и возвращаем результат
func ParseFile() { ... }

// BAD: context.Context stored as struct field (staticcheck SA1029)
type svc struct {
    ctx context.Context // never do this
    db  *sql.DB
}

// BAD: "failed to X: %w" — redundant "failed" in every wrap layer
return fmt.Errorf("failed to parse %s: %w", path, err)

// BAD: ignoring json.Unmarshal error
_ = json.Unmarshal(raw, &out)
```

**Do:**

```go
// GOOD: Interface + private impl
type Parser interface { ... }
var _ Parser = (*svc)(nil)
func New() Parser { ... }

// GOOD: Error context
return fmt.Errorf("parse %s: %w", path, err)

// GOOD: Structured logging via zap, named + ctx-scoped
log := logger.Ctx(ctx).Named("parser.parse")
log.Info("parsing", zap.String("path", path))

// GOOD: Small focused functions
func (s *svc) extractFunctions() { /* 30 lines */ }
func (s *svc) extractMethods() { /* 25 lines */ }

// GOOD: Flat structure with early returns
if !valid {
    return nil, ErrInvalid
}
result := process(data)
return result, nil

// GOOD: Private by default
type svc struct { ... }
func newHelper() { ... }

// GOOD: English comment
// handles edge case when file is empty
func (s *svc) handleEmpty() { ... }
```

---

## Architecture-level style

Prescriptive structural rules for new code: layering, package shape, dependency injection, lifecycle, persistence, and cross-package contracts. Rules phrased in terms of `fx` apply equally to coagent's hand-written composition root — read "the composition root" for "fx" throughout. This part does not inventory the project; the actual package-to-tier mapping and contracts live in [ARCHITECTURE.md](../ARCHITECTURE.md).

### §1. Layering

**1.1 — Declare tiers.** Every package belongs to exactly one tier. Canonical taxonomy:

- **transport** — WebSocket / HTTP servers, protocol adapters, session managers, connection handlers.
- **domain** — per-task orchestration, business logic, state machines, bounded-context services.
- **infra** — external integrations (LLM clients, databases, caches, tool implementations, MCP servers, LSP clients).

Imports flow **downward only**: transport → domain → infra. Peer imports within a tier are allowed for shared infra. Upward imports are forbidden.

Enforce in CI — a simple grep across the tier directories is sufficient:

```bash
# higher-tier package names must not appear in lower-tier source
rg -l "<higher-tier-pkg-names>" <lower-tier-dirs> && exit 1 || true
```

Project-specific mapping (which package lives in which tier) belongs in the project's `ARCHITECTURE.md`, not here.

**1.2 — Leaf utility packages must not import components wired by the composition root.** Logger, config, ID generation, CLI wrappers, embedded migrations, shared DTOs — these have no dependencies on the business-logic tree. This is enforceable with a grep in CI (or a tool like `go-arch-lint`, if the project uses one — see the project's own `.go-arch-lint.yml` for the enforced edges).

---

### §2. Package shape

**2.1 — One primary interface per package.** Each package exports one primary
behavior interface (`Service`, `Client`, `Registry`, `Store`, `Manager`,
`Factory`, `Executor`, `Pool`). Secondary narrow role interfaces are allowed
when they are genuine dependency-inversion points (`Spawner`, `Compactor`,
`SessionSender`) or when a package intentionally defines a small protocol
family (`tool.Tool` + `tool.Registry`). The implementation is private (`svc` or
a purpose-named lowercase struct). Compile-time check:

```go
var _ Service = (*svc)(nil)
```

**2.2 — `New()` returns the interface, never `*concrete`.** Concrete-pointer returns are allowed only for leaf data types with no behavior surface. A type exposing lifecycle, side effects, or substitutable behavior must go behind an interface.

Reason: consumers program against the contract; tests mock the interface; `var _` is the single source of truth for conformance.

---

### §3. Dependency injection

**3.1 — Constructors take positional arguments.** No Param-grouping structs — neither `fx.In` / `fx.Out` tag structs nor plain DI-bag structs that collect every dependency into one exported type.

```go
// GOOD (fx-wired)
func NewExecutor(lc fx.Lifecycle, store Store, sender Sender) Executor

// GOOD (manual composition root — no lc; the root calls Start/Stop itself)
func NewExecutor(store Store, sender Sender) Executor

// BAD — fx.In forces exported struct, collides with other Params types
type Params struct {
    fx.In
    Store  Store
    Sender Sender
}
func NewExecutor(p Params) Executor

// ALSO BAD — plain Param bag hides the dependency list from grep and tests
type Params struct {
    Store  Store
    Sender Sender
}
func NewExecutor(p Params) Executor
```

Reason: positional args make the dependency graph grep-able, fail loudly when a
dep is added, and keep the wiring identical to how tests call the constructor
directly — true whether the composition root is `fx` or a plain `main.go`.
Existing broad bags such as `session.Params` are migration debt, not a
shape to copy.

**3.2 — `cmd/coagent/main.go` is the composition root** — either the fx root or,
for a manual composition root, the place every component is constructed by hand
in dependency order. DI wiring is centralized and visible in the binary
entrypoint. Leaf packages expose constructors, not `var Module = fx.Options(...)`
(fx) or package-level wiring helpers (manual), unless a subsystem has grown
large enough that a small wiring-only module removes real noise from
`cmd/coagent/main.go`.

```go
// fx-wired
fx.New(
    shared,
    fx.Provide(migrate.ProvideDB),
    fx.Provide(daemon.NewStore),
    fx.Provide(session.NewStore),
    fx.Provide(session.NewFactory),
    fx.Provide(daemon.New),
    fx.Invoke(managers.Start),
)

// manual composition root — construct in order, record a named stop closure
// per component as it starts, replay in reverse on shutdown
db, err := migrate.Open()
a.onStop("db", func(ctx context.Context) error { return db.Close() })
store := daemon.NewStore(db)
factory := session.NewFactory(cfg, ...)
svc := daemon.New(factory, store, ...)
svc.Start(ctx)
a.onStop("daemon", func(ctx context.Context) error { svc.Shutdown(30 * time.Second); return nil })
```

Adding a provider (or a construction line) usually touches the composition
root. That visibility is a feature for coagent: wiring changes are easy to
review and grep. If composition eventually becomes too large, split by
subsystem into wiring-only modules; do not add business logic to wiring
packages. A manual composition root's shutdown order is simply the reverse of
its named-stop registration order — no dependency-graph resolution needed,
but also no automatic enforcement: get the registration order right by hand.

**3.3 — Side-effect roots invoked directly by the composition root return `error` (or nothing).** New
side-effect roots should not return `(*T, error)` where `*T` is unused. This
applies equally to `fx.Invoke` functions and to a manual composition root's
top-level calls (e.g. `runtime.Start(ctx) error`).

A `(*T, error)` return signals "something depends on this." If nothing does,
use an `error`-only signature and attach lifecycle hooks internally (fx) or have
the composition root call `Start`/`Stop` explicitly and register a stop closure
(manual). Existing invoked constructors that return unused concrete values are
cleanup targets.

---

### §4. Lifecycle

**4.1 — Accept a lifecycle hook (fx) or expose explicit `Start`/`Stop` (manual) iff you own background work.** A constructor takes `fx.Lifecycle` — or, in a manual composition root, the resulting object exposes `Start(ctx) error` / `Stop(ctx) error` (or `Shutdown(timeout)`) for the root to call explicitly — if and only if the resulting object owns at least one of:

- a goroutine it spawns
- a subprocess
- a network listener
- a pooled external connection

Pure-compute / stateless services (registries, parsers, value-object services) do not take `lc` and do not need `Start`/`Stop`.

In an fx-wired project, hooks are registered inside the constructor or inside
the invoked side-effect root that owns the lifecycle — the composition root
should not reach into a service's internals to attach hooks. In a manual
composition root, the root itself calls `Start` right after construction and
registers a named stop closure calling `Stop`/`Shutdown` — see §3.2.

**4.2 — Start/Stop (or `OnStart`/`OnStop` under fx) are paired.** `Stop(ctx)` accepts context; never `Stop(timeout Duration)` on the interface (a `Shutdown(timeout)` variant that wraps a context-based `Stop` internally is fine). The shutdown budget belongs to `fx.StopTimeout` at the app root, or to an explicit timeout the manual composition root passes into its own top-level shutdown context — services must not invent their own duplicate budgets.

**4.3 — Long-lived goroutines derive ctx from `context.Background()` inside the constructor or start hook.** Store `cancel context.CancelFunc` and a `done chan struct{}` on the receiver. `Stop()` cancels, then blocks on `<-done`.

Never capture the start hook's ctx for work that outlives startup — under fx it may be cancelled after the hook returns; in a manual composition root the equivalent mistake is capturing a request-scoped or startup-scoped ctx instead of deriving a fresh one from `context.Background()`.

**4.4 — `go` statements are scoped.** A `go func()` appears only in:

- a constructor (starting a lifecycle-bound goroutine)
- a `Start()` method
- an explicit per-request fan-out function that owns the result channel

Every spawned goroutine has a defined cancellation path at the call site. Ad-hoc `go func()` in business logic is forbidden.

**4.5 — Mutex strategy is one per struct.** A struct holding shared state uses ONE of:

- (a) single `sync.RWMutex` for read-mostly maps
- (b) single `sync.Mutex` for write-heavy state
- (c) channel-based actor (single owning goroutine, inputs via chan)

Multi-mutex structs require a doc-comment declaring lock order. `atomic.*` is reserved for single-word shutdown/counter flags. Never mix actor and mutex styles on the same struct.

**4.6 — Long-lived goroutines recover from panics.** Any goroutine that outlives a single request (session runners, pool reapers, fan-out subscribers, schedule tickers) wraps its main body in:

```go
defer func() {
    if r := recover(); r != nil {
        log.Error("goroutine panic",
            zap.Any("recovered", r),
            zap.Stack("stack"),
        )
    }
}()
```

Per-iteration / per-request goroutines (e.g., one tool-call execution inside a larger loop) may rely on the caller's recovery scope, but a long-lived background goroutine MUST recover — unhandled panics in background work kill the daemon silently.

---

### §5. Persistence

**5.1 — One `Store` per bounded context.** Soft cap: **≤15 methods, ≤2 aggregate roots.** When a Store exceeds this, split it. A Store never writes to tables owned by another Store.

**5.2 — Transactions live at the lowest layer that owns the whole invariant.** A Store may expose an atomic command when every participating table belongs to that persistence boundary; callers must not reconstruct that transaction as a sequence of CRUD calls. If an invariant genuinely spans independently owned Stores, compose it in the service/use-case layer through an outer `Transactor` and pass the shared `*sql.Tx` into each Store.

`BeginTx(ctx, nil)` everywhere. Plain `Begin()` is forbidden — it drops ctx cancellation.

**5.3 — Single DB handle.** The `*sql.DB` / `*pgxpool.Pool` is provided once at the app root and closed via an fx lifecycle hook or, in a manual composition root, an explicit stop closure registered right after it's opened. No package opens its own connection; no global `var db`.

**5.4 — One SQL-access style per project.** Pick sqlc, raw `database/sql`, or a query builder — and stick to it. Shared scan / null-value helpers live in one helper package; never duplicated across Stores. Mixing styles without an explicit escape-hatch convention is a smell.

---

### §6. Cross-package contracts

**6.1 — Producer-side interfaces by default.** The package that implements the interface also defines and exports it. Typical names: `Service`, `Client`, `Registry`, `Store`.

**6.2 — Consumer-side interfaces only when justified.** Use a consumer-side interface only when:

- **(a)** a producer-side interface would cause an import cycle, OR
- **(b)** the consumer needs a narrow slice (≤3 methods) of a much broader contract.

Consumer-side interfaces are named by the **role** they fulfill, not the type they mirror (`Compactor`, `Sender`, `Runner` — not `SessionLike`).

Never mix producer-side and consumer-side interfaces for the same type within one project.

**6.3 — Cross-package data types live in the lowest common ancestor.**

- If only one consumer: stays in producer package.
- If a type crosses a tier boundary (transport/domain/infra): promote to a shared leaf package.
- If 3+ unrelated consumers: promote.

Persistence-only fields (DB row IDs, internal timestamps, `json:"-"` flags) **never** appear in shared wire DTOs. Use separate persistence structs; convert at the boundary.

**6.4 — Shared types are split by role.** Never one mega-package. Split into: wire shapes (DTO), business entities (domain), typed primitives / enums. A single `dto/` folder with 80 unrelated types is a smell.

---

### §7. Error architecture

**7.1 — Propagate by default.** Log-and-continue is allowed in exactly three cases:

- **(a) Cleanup / shutdown path** where no one can act on the error — use `Warn`.
- **(b) Post-commit side effect** where the primary work already succeeded (PubSub publish after DB commit, Slack notification, event emission) — use `Error` with context.
- **(c) Optional enrichment** with a sane fallback already in place — use `Warn`.

**Never log-and-continue on a primary durability write.** Persistence failures on the request's hot path must propagate — silent drops turn bugs into data loss.

**7.2 — Sentinel errors are declared in `<pkg>/errors.go`, only when a caller branches via `errors.Is`.** Not proactively. Typed errors (`type FooError struct { ... }`) only when the error must carry structured fields.

Wrap syntax and retry semantics: see [Code-level style → Error Handling](#error-handling).

---

### §8. Logging architecture

**8.1 — One acquisition path per call site.** Chosen by scope:

- **If you have `ctx` — use `logger.Ctx(ctx)`**. Always. Even in "background" code. The context may carry enriched logger with correlation IDs, session info, trace spans. `log := logger.Ctx(ctx).Named("<pkg>.<file>")` at function entry.
- **No ctx available** (constructors, `OnStart` hooks before ctx exists, top-level init): `log := logger.Named("<pkg>.<file>")` at function entry.
- **Struct-field logger** (`log *zap.Logger` on the struct): only when the struct has stable context across many methods and is constructed once.
- **Bare `logger.L` is forbidden** outside the `logger` package itself. Every log line carries a `Named` prefix.

Repeated inline `logger.L.Named("X").Warn(...)` calls in the same function are forbidden — hoist to one `log :=` at function entry.

**8.2 — Logger names are dotted.** Format: `<package>.<file_or_func>`.

Good: `pool.reaper`, `store.insert`, `handler.create`, `worker.tick` — each identifies the exact source.

Bad: flat single-token (`"worker"`, `"store"`) shared across many files — destroys grep-filtering.

---

### §9. What NOT to do

Patterns observed in mature Go services we explicitly do **not** import:

- **God-repo** — one interface with 200+ methods spanning every domain. Always split by bounded context (§5.1).
- **Hidden mega-module wiring** — one package-level `services.go` file with
  `fx.Provide(40 constructors)` + ordering comments (`// keep it last`) and no
  business context. Keep coagent wiring visible in `cmd/coagent/main.go`; split
  only into small wiring-only subsystem modules when that improves reviewability
  (§3.2).
- **Runtime tx-only type assertions** (`r.db.(*sql.Tx)`) — compile-time split is better than runtime panic.
- **Fat producer-side interfaces consumed wholesale** — declare a narrow consumer-side interface (§6.2) instead of importing a 40-method surface.
- **Entities with all fields public and mutable across package boundaries** — no read-only view type. Encapsulate via methods or convert to value copies at boundaries.
- **Vestigial / placeholder packages** — empty directories, `.tmp` files, stub files "for future use". Either populate or delete.

---

### §10. API evolution & removals

**10.1 — Pre-1.0 rule: remove in one PR.** When deleting or renaming a public interface, exported function, or package, update every consumer in the **same commit**. Do not leave stubs that panic / no-op / return zero values. Do not parallel-version. Do not add `//lint:ignore` shims. A grep for the old name should return zero hits after the PR lands.

**10.2 — Deprecation comments are for post-1.0 only.** Pre-1.0 projects land breaking changes inline. Once the project commits to stability, add `// Deprecated: use NewThing instead.` per Go convention and schedule a removal commit in a separate PR at least one release later.

**10.3 — Contract changes propagate through the type system.** If a public interface grows or changes signature, update all implementors in the same PR. Compile errors are the intended signal — silence them rather than by implementing no-op methods on stale mocks. Tests that go stale in the process are fixed, not skipped.

**10.4 — ARCHITECTURE.md updates are part of the change.** If a PR modifies a package's contract, invariants, state ownership, or concurrency model, update that package's section in the project's single `ARCHITECTURE.md` in the same PR. Stale architecture docs are worse than none — reviewers start to distrust the whole file.
