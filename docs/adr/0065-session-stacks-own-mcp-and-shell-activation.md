# ADR-0065: Session stacks own MCP processes and shell activation

- **Status:** Accepted
- **Date:** 2026-09-24
- **Supersedes:** [ADR-0045](0045-session-bound-mcp-process-identity.md)

## Context

The daemon kept three overlapping lifetimes for a session's tools: a tool stack,
an idle pool of MCP subprocesses and catalogs, and a network generation. Shell
activation also had a daemon-wide snapshot cache. An idle MCP subprocess could
hold its network namespace after the stack closed, so the gateway's ten-minute
idle period began only after the pool's separate thirty-minute timeout. Policy
retirement had to coordinate all of these owners.

## Decision

Each tool stack creates and owns its shell-activation provider, MCP clients,
LSP manager, file access and network-generation lease. MCP discovery happens
when the stack is built. Closing the stack stops its MCP and LSP processes,
removes its shell snapshots and releases the network lease. A background process
may still retain the network generation independently until it exits.

The daemon no longer pools live MCP clients or caches their tool catalogs.
Registry mutations remain durable and take effect on the next stack; they do
not rewrite the tools of an active stack. The next stack discovers the server's
current `tools/list` even if the configuration row did not change.
Lifecycle transcript settlement opens stored messages without constructing a
tool stack or model client, so stopping a tree cannot launch new MCP code.

## Consequences

- One stack has one process and activation lifetime, with no MCP idle reaper,
  catalog expiry or cross-owner policy retirement.
- Each new stack may pay for MCP startup and discovery before its first model
  request. An unavailable server is omitted from that stack's tools.
- Tool schemas remain stable within a stack but may change between stacks.
- The network generation still has its own ten-minute idle retirement because
  background processes can outlive a tool stack.

## Alternatives Considered

- **Keep the live pool with explicit network leases.** This preserves warm MCP
  clients but retains another owner, timer and retirement protocol.
- **Keep only a catalog cache.** This avoids some startup waits but allows a
  stack to advertise stale tools and still needs invalidation and expiry.
