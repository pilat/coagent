# coagent

A headless coding agent daemon. Tasks come in from outside the terminal, the
agent runs a plan/act loop with tool calling until it is done, and the result
goes back out. No TUI, no editor plugin, no web UI.

Early stage — most of the tree is still being sketched out.

## Commands

```bash
make build      # go build -o coagent ./cmd/coagent
make test       # go test ./...
make lint       # golangci-lint run ./...
make fmt
make all        # fmt + build + lint + test

# one test
go test ./internal/logger -run TestRedact -v
```

Go version is pinned in `mise.toml`.

## Layout

- `cmd/coagent` — entrypoint and wiring
- `internal/coagenthome` — the single owner of `~/.coagent` path resolution
- `internal/logger` — zap wrapper, plus credential redaction
- `internal/config` — configuration loading

Package tree under `internal/` stays flat. Package names say what they provide;
do not add directory nesting to express layers.

## Style

- Export interfaces, not structs. `var _ Iface = (*impl)(nil)` compile-time check.
- `New()` returns the interface, the implementation struct is private (`svc`).
- File order: `const` -> `var` -> `type` -> exported funcs -> unexported funcs.
- Flat error handling. No nested `if err != nil { if errors.Is(...) }` ladders.
- Table-driven tests with testify `assert`/`require`.
- Comment the why, never the what.

## Configuration

`~/.coagent/config.yaml` for everything non-secret, `~/.coagent/secrets` for
credentials. Nothing is loaded from the working directory — a task repository
must not be able to inject settings into the daemon.
