# Coding Style

House style for Go in this repo. Prescriptive: it says how new code should look.
Existing violations are debt, not precedent.

## Project decisions

- `go.uber.org/zap` behind `internal/logger`. No `fmt.Println` in library code.
- SQLite with raw `database/sql`. No ORM.
- Hand-written test doubles. No generated mocks.
- No DI framework. Composition is a hand-written root in `cmd/coagent/main.go`.

## Package shape

Interface at the top of the file, private implementation below, compile-time
check in between:

```go
type Store interface {
    Get(ctx context.Context, id string) (*Record, error)
}

var _ Store = (*svc)(nil)

type svc struct{ db *sql.DB }

func New(db *sql.DB) Store { return &svc{db: db} }
```

- One package, one responsibility. No `package main` outside `cmd/`.
- Name the file after the package (`logger/logger.go`).
- Leaf packages must not import component packages.

## File layout

Declaration order inside a file: `const` -> `var` -> `type` -> exported
functions -> unexported functions. Keep declarations at the top, not scattered
between functions.

Soft limits: files under 300 lines, functions under 50.

## Errors

- Wrap with context: `fmt.Errorf("open db: %w", err)`.
- Sentinel errors (`var ErrNotFound = errors.New(...)`) plus `errors.Is`. Never
  compare error strings.
- Flat checks. `errors.Is` first, then the general `if err != nil`. No ladders.
- New code propagates errors. `_, _ = db.Exec(...)` is not acceptable.

## Concurrency

- Never hold a mutex across IO, DB or network calls. Copy under the lock,
  release, then do the work.
- Every goroutine needs a defined shutdown path. A detached `go func()` with
  `context.Background()` is a leak.

## Tests

Table-driven, `testify/require` for preconditions and `testify/assert` for
expectations. Tests must not touch the real `$HOME`.

## Comments

Comment the why, never the what. If deleting the comment loses nothing a reader
could not recover from the code itself, delete it. Two lines, maximum.
