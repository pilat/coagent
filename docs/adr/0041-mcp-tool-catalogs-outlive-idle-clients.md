# ADR-0041: MCP tool catalogs outlive idle clients

- **Status:** Accepted
- **Date:** 2026-09-04

## Context

The daemon pools workdir-bound MCP subprocesses. A session stack acquires every enabled MCP server while it is being built, and tool definitions are available only from the live client returned by `tools/list`. Once the last stack releases an idle client, the pool reaps it after 30 minutes. The next session activation must then start every configured MCP server and wait for discovery before sending its first model request.

That wait delays user input even when the model will not use MCP. Starting a reaped client later and replacing its definitions in the active registry is worse: both the tool schemas and the tool inventory contribute to the model request and can invalidate a long session's provider prompt cache. Direct per-tool schemas remain important; a generic search/proxy tool would make weaker models less reliable.

## Decision

We keep pooled live MCP clients and add a daemon-memory catalog cache to the MCP pool.

A catalog is keyed by the same resolved server-config/workdir hash as its live client. It contains copied tool name, description, and serialized input schema only; it never owns or references a client, transport, subprocess, context, or cancellation function. Live clients remain idle for 30 minutes. Unused catalogs remain for 15 days, then the pool reaper removes them. Both disappear when the daemon stops.

A catalog miss retains the current synchronous `initialize` and `tools/list` path before the first model request. A catalog hit can build direct MCP tools without starting a reaped client. The tool starts/acquires its missing live client only when the model actually calls it. That reconnect never changes the activation's catalog or schemas, even if its fresh `tools/list` differs.

The MCP registry tools are intentional catalog boundaries. After each successful add, enable, remove, or disable mutation, the pool invalidates cached metadata for that server name and retires matching live clients with its existing safe, close-after-release lifecycle. Changes remain visible from the next session activation, never the current one.

## Consequences

- A known MCP process can be reaped without delaying the next model request or changing its direct MCP tools.
- A first daemon use, expired catalog, or daemon restart still waits for discovery once and receives current definitions.
- An MCP server that changes its tools without a registry mutation remains model-visible through its previous definitions until restart or catalog invalidation. A removed tool can therefore fail normally when invoked after lazy reconnect.
- The pool owns two bounded in-memory resources with separate lifetimes: executable clients and non-executable catalog metadata.
- Invalidation must track all server names associated with a configuration hash. Matching aliases must not leave a stale catalog behind.
- This supplements ADR-0004. SQLite remains the source of truth for server definitions and registry mutations continue to take effect from the next activation.

## Alternatives Considered

- **Restart every MCP before every activation.** Rejected because it blocks the first model request after idle even when no MCP tool will be used.
- **Remove the live process pool.** Rejected because it would lose common sequential reuse in the same workdir, failure cooldown, and safe shared ownership; a lazy executor would need to recreate much of that lifecycle machinery.
- **Persist catalogs in SQLite or on disk.** Rejected because restart is an intentional freshness boundary and durable cached schemas add migration, stale-data, and secret-identity concerns without serving the current goal.
- **Dynamically add freshly discovered MCP tools to a running activation.** Rejected because it changes the model-visible schemas/prompt and repeatedly invalidates provider cache prefixes during active work.
- **Use a generic MCP search/proxy tool.** Rejected because it weakens direct schema guidance and makes MCP use less dependable for weaker models.
