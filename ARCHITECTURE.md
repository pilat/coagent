# coagent — Architecture

First pass at writing down what the packages are and which way imports are
allowed to point. Incomplete on purpose: it covers the packages that exist
today, and it gets extended as the rest lands.

> **Scope**: the Go code under `cmd/` and `internal/`.

## Tiers

The tree under `internal/` is flat. Package names say what they provide; the
layering is a property of the import graph, not of directory nesting.

```
leaf        coagenthome, logger, id, llmwire, sessionevent, controllerapi,
            version, git, tool, registry
            ^ import nothing from the component tier, ever

support     config, catalog, shellenv, migrate, todo, memory
component   llm, loader, mcp, mcpstore, lsp, ctl, configops, bashsandbox,
            sessionstore
```

An import from a leaf package into a component package is a bug, not a
shortcut. When something in a leaf needs a component's type, the type belongs
in the leaf.

## Package ownership

| Package | Owns |
|---------|------|
| `internal/coagenthome` | `~/.coagent` resolution and the name of every file inside it |
| `internal/logger` | zap setup, the human encoder, credential redaction |
| `internal/config` | YAML config parsing and secrets resolution |
| `internal/controllerapi` | the Controller contract and its DTOs |
| `internal/sessionevent` | the session -> controller event vocabulary |
| `internal/llmwire` | the LLM wire types shared by everything that talks to a model |
| `internal/llm` | provider drivers, retries, cost accounting |
| `internal/catalog` | model catalog fetch, disk cache, id matching |
| `internal/tool` | the Tool/Result/Registry contract — protocol only, no tools |
| `internal/loader` | CLAUDE.md, SKILL.md, subagent definitions, marketplace repos |
| `internal/mcp` | MCP connections, pooling, tool discovery |
| `internal/mcpstore` | the MCP server registry table |
| `internal/lsp` | language server lifecycle and code intelligence queries |
| `internal/ctl` | the control socket: JSON-RPC over `~/.coagent/daemon.sock` |
| `internal/configops` | config mutations: guards, atomic write, backups |
| `internal/bashsandbox` | native write confinement (Seatbelt, Bubblewrap) |
| `internal/shellenv` | per-cwd login-shell snapshots for tool subprocesses |
| `internal/sessionstore` | session and message persistence, delivery CAS |
| `internal/memory` | curated per-project memory |
| `internal/migrate` | SQLite open plus the goose migration runner |
| `internal/git` | git CLI wrapper |

`internal/sessionstore` is the only package that writes the message schema.
Anything that needs conversation state goes through it.

## Rules for new code

1. **Persist before any side effect.** If the write fails, abort. No
   fire-and-forget.
2. **On crash, only what is in SQLite survives.** Design so that resuming from
   the database produces correct behavior.
3. **Never hold a mutex across IO.** Copy what you need under the lock, release
   it, then do the call.
4. **Every goroutine needs a shutdown path.**
5. **Propagate errors.** Sentinels plus `errors.Is`, never string comparison.
6. **Every acquired resource gets cleanup on every exit path.**

## Configuration

`~/.coagent/config.yaml` is strict: an unknown key is a fatal error, so a
misspelled setting cannot silently take a zero value. Credentials live in
`~/.coagent/secrets`, are read into an in-memory map, and are never exported
into the environment that subprocesses inherit.

## Keeping this accurate

This document describes what the code *is*. When a package changes shape, this
file changes in the same commit. Style rules — how a line is written — live in
[docs/coding-style.md](docs/coding-style.md).
