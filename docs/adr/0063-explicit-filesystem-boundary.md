# ADR-0063: Explicit filesystem confinement on the operator's host

- **Status:** Accepted
- **Date:** 2026-09-26
- **Supersedes:** [worktree filesystem grants](0042-work-tree-sessions-write-the-main-repository-git.md), [session shields](0044-session-shields-confine-project-filesystem.md), [pooled session-bound MCP processes](0045-session-bound-mcp-process-identity.md)

## Context

Coagent works in the operator's existing development environment. Toolchains,
SSH agents, shared databases and locally started development servers are part
of that environment. Enumerating supported tools, credential locations and
writable caches in coagent turned ordinary setups into a growing collection
of product-specific exceptions.

We also implemented network isolation with userspace forwarding and then
kernel-routed namespaces. The latter added capabilities, host configuration and
network lifecycle ownership while changing what `localhost` means and whether
the operator can reach a session's development server. Public egress still had
to remain available, so the boundary primarily protected host and LAN services.
Implementation bugs were fixable; the lasting problem was the installation and
behavioral cost imposed on every operator for that narrower protection.

The boundary should therefore express explicit filesystem permissions while
preserving the host development environment. Stronger isolation belongs to the
deployment chosen by the operator.

## Decision

Use one compiled filesystem policy for each sandbox-enabled session. The host
is read-only and the project is read-write by implicit default. Operator-authored
`allow` and `deny` rules apply in order, with the last matching rule deciding:
global default, global rules, project defaults, then project rules. Coagent ships
no credential list, writable-cache list, private temporary directory or tool
profiles. Remove the separate session-shields state and commands.

Resolve rule paths and project keys to canonical absolute paths and reject
duplicate project aliases. Ignore wholly superseded rules when mounting. Refuse
an effective deny whose target is absent rather than silently opening that path
or creating a placeholder on the host. Refuse absent read-only exceptions beneath
writable grants too. Check this again before each process launch.

End the project section with mandatory read-only access to its process-output
directory, after its operator rules. These daemon-owned files must remain
inspectable by roots and subagents even when the operator denies coagent's home;
sessions receive no write access or exception for other daemon state.

A `/gwt` worktree inherits its source project's rules and may add its own. Its
shared repository `.git` is writable by default because Git operations require
it; this is not branch-level write isolation.

The same policy governs Bash, LSP, stdio MCP, shell-environment capture and
built-in file tools. A tool-resource owner retains each session ID's shell
snapshots and MCP clients across loop activations, isolated from every other ID.
Its generation includes canonical workdir, policy and MCP configuration. Each
stack leases these resources and owns its LSP manager, file access and registry.
Policy/configuration changes retire resources; shell recapture replaces MCP.
Stop/kill retire a session tree and shutdown closes the owner. MCP catalogs stay
with their live clients and refresh before reuse. Background processes retain
their separate lifecycle.

Sessions share the daemon's network namespace. Coagent provides no network
filtering, port publishing, network grants or traffic cutoff. The web tools'
own destination checks remain local to those tools. An operator needing a
stronger boundary isolates the whole daemon in their deployment.

## Consequences

- Default confinement limits filesystem damage; it does not hide credentials
  or prevent disclosure. Operators must name paths to deny. Filesystem rules
  do not filter inherited environment variables or authority behind an allowed
  service socket.
- Host services and the LAN remain reachable, and development servers remain
  accessible from the host. Sessions share its port space. No additional network
  capabilities, forwarding settings or routing tools are required.
- Session-owned tool resources amortize shell activation and MCP startup across
  replies and sleep/resume. Active stacks keep fixed schemas; registry changes
  retire idle resources immediately and active resources after release.
- Linux and Bubblewrap remain the supported runtime under
  [ADR-0062](0062-linux-only-supported-runtime.md). `sandbox.enabled: false`
  explicitly disables filesystem confinement.

## Alternatives Considered

- **Tool profiles or built-in protected paths:** rejected because they encode
  assumptions about installations and secret locations that coagent cannot own.
- **A second shields mode:** rejected because operator rules already express
  narrower access without another durable state and transition protocol.
- **Keep network isolation, optionally or without filtering:** rejected because
  namespaces still change host-service and port semantics; making it optional
  preserves two behavioral contracts and their maintenance burden.
- **Share MCP clients or retain catalogs without their clients:** rejected
  because sessions have distinct authority and a catalog can advertise tools
  that the next server instance no longer provides.
