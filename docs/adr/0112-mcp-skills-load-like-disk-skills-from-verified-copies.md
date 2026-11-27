# ADR-0112: MCP skills load like disk skills from verified copies

- **Status:** Accepted
- **Date:** 2026-11-26
- **Amends:** [ADR-0064](0064-untrusted-tool-output-is-data.md)

## Context

The [Skills over MCP extension](https://github.com/modelcontextprotocol/ext-skills/blob/main/specification/beta/skills.mdx)
(SEP-2640, `io.modelcontextprotocol/skills`) lets an
MCP server supply skill metadata, SKILL.md and supporting files. Coagent previously
loaded skills only from disk and marketplaces. The MCP SDK, mcp-go, has no skills
methods in v1.5.0 or v1.6.0, so using the extension also requires a transport-level
request for its listing.

Coagent is headless and single-operator. The extension's security rules assume an
interactive host that can ask for per-skill approval. Coagent needs an explicit
trust decision that works unattended while preserving origin identity, lazy reads
and consistency with the server's manifest.

## Decision

1. **Enabling a server is the trust act.** Its skills and supporting files have
   the same instruction weight as disk skills, need no approval and receive no
   untrusted-output wrapper. Scripts may run under the session's confinement
   policy. This deliberately departs from the specification's MUSTs treating
   skill content as untrusted input, treating skills as a higher-risk surface
   than remote tool invocation, and requiring explicit per-skill approval for
   local execution. The operator already trusts the server to run as a process;
   an approval exchange cannot serve unattended execution. The activation
   envelope identifies the operator-installed server and states that the skill
   is not MCP tool output. `allowed-tools` remains ignored, as for disk skills.

   Two other deliberate departures are the held entry's lifetime of one loop
   activation, rather than the full lifetime of SKILL.md in context, and the
   absence of `skills/get`: a skill omitted from the listing cannot load by URI.
   Durable per-skill manifests would add state for a distinction that this
   single-operator trust model does not require.
2. **Canonical names are `<label>:<name>`.** The label is the server's registry
   name with bytes outside `[A-Za-z0-9_-]` replaced by `_`. Within one server,
   duplicate skill names all use their skill root without `scheme://` as the
   suffix. Across servers, label ownership is determined from all server
   definitions in `StackConfig.Servers`, sorted by registry name, before session
   visibility: the first keeps the label and later servers offer no skills, even
   if the first fails to start. Canonical names therefore never change origin
   between parent and child stacks of the same generation. Disk, marketplace and
   system skills register first and keep a colliding name; MCP skills never
   overwrite them. If a bare suffix matches MCP skills and exactly one non-MCP
   skill, it resolves to that skill. Declines and name collisions are logged.
3. **Visibility extends ADR-0089 through the same computation.** Visible MCP
   tools declare their server, and guidance and skill visibility both use the
   `scanPromptTools` function. A session with the skill tool sees a server's skills
   when one of that server's tools is visible. A server with no tools after its MCP
   profile, before agent-type filtering, is the exception: every session with
   the skill tool sees its skills. Removing a server's tools through an agent
   allowlist does not make it a skills-only server. Sessions without the skill
   tool register no MCP skills.
4. **Freshness follows the per-activation catalog refresh.** Only a server
   declaring the extension is asked for a paginated `skills/list`. Listing
   failure clears its skills and logs a warning while retaining its tools.
   Invalid entries are declined individually. TTL and cache-scope hints create
   no second freshness mechanism.
5. **Files are fetched only on demand.** SKILL.md is read for activation through
   `/skill`, the skill tool or task seeding. Each read checks byte size and
   SHA-256 against the listing; the loader also compares SKILL.md frontmatter
   with the listing through JSON-normalized values. A mismatch fails without a
   refresh retry. Digests prove listing consistency, not a security boundary.
6. **Verified copies live beneath the project's process-output directory.**
   Paths include the escaped registry name and the URI's scheme, authority and
   decoded path segments. If any manifest URI cannot map safely to a local path,
   the whole skill is declined during stack construction, before first use. The
   daemon writes a temporary file and renames it into place with mode `0o500`,
   so scripts can execute directly without a write bit. This directory is already
   read-only under confinement, including after `deny: ~/.coagent`, and lies outside skill
   discovery paths. Every host use rechecks the stored bytes. Parent, child and
   restarted sessions of one project share the same paths.
7. **Supporting files open through the skill tool's `file` argument.** An exact
   canonical name and a listed relative path return verified content and a local
   path without an activation receipt. Model-hidden MCP skills remain readable
   this way. Binary content returns only its path; a nested SKILL.md is ordinary
   supporting content, with no frontmatter effect.
8. **Dynamic manifests are declined.** They cannot satisfy the complete
   size-and-digest manifest rule. Neither `skills/get` nor
   `resources/directory/read` is used.
9. **Missing SDK methods use the existing transport.** `skills/list` uses string
   request IDs, distinct from the SDK's numeric IDs, and includes protocol,
   implementation and empty client-capabilities `_meta` on 2026-07-28 connections.
   Legacy requests omit that metadata. Each call is bounded; timeout sends no
   cancellation notice. SDK `resources/read` handles file reads. The SDK stays
   at v1.5.0.
10. **Manager discovery lists disk skills only.** Telegram `/help` and
    `DiscoveryService.ListSkills` do not depend on retained MCP clients. A typed
    `/skill <server>:<skill>` works, and the root's own in-session `/help` lists
    its visible MCP skills.

## Consequences

- MCP skills use the existing invocation, task-seeding, receipt, compaction and
  restart paths. There is no new durable skill state or migration; compaction
  retains the activation envelope byte-for-byte.
- Disabled, failed and no-longer-listing servers give the ordinary
  `skill unavailable` rejection; logs distinguish the cause. Unlisted skills,
  dynamic manifests and invalid Agent Skills names remain unavailable.
- A mid-activation change fails verification until the next activation relists.
  After restart, compaction or a server update, supporting-file reads may use a
  newer manifest while the model still follows older SKILL.md instructions.
- YAML-only frontmatter values such as timestamps and non-string keys fail
  JSON-based verification. Percent-encoded URI suffixes appear unchanged in the
  envelope while local paths use decoded segments.
- Stored copies are never deleted, including after server removal. Concurrent
  sessions using different versions may replace a shared path before another
  session executes it. With confinement disabled, a model can modify copies;
  host re-verification cannot verify what Bash executes.
- There is no per-server skill cap. Every supporting path appears in the
  envelope, up to 511 lines at the file limit. Binary files show no content.
- Sanitized-label collisions hide the later server's skills; a colliding
  marketplace skill keeps its name. Both cases produce warnings.
- The skill tool's schema includes `file` in every session with that tool, even
  with no MCP skills. Using it with a disk skill returns an error.
- A task-seeded child that cannot see the originating server retains the
  instructions but cannot open its files. A type without the skill tool has no
  MCP skills of its own, including for task seeding.
- First activation adds a lazy SKILL.md read bounded by the five-minute call
  timeout; verified copies serve subsequent reads. Listing timeout does not
  send `notifications/cancelled` on modern connections.
- The approval and untrusted-content departures are intentional: enabling a
  server authorizes its skill instructions and script execution under the
  configured confinement policy.

## Alternatives Considered

- **An approval prompt:** rejected because unattended execution has no person
  available to answer; the operator chooses trust when enabling the server.
- **Wrap skill content as untrusted output:** rejected because the operator
  requires the same instruction weight as disk skills.
- **A separate supporting-file tool:** rejected because it duplicates the skill
  flow and needs a new grant in every agent type.
- **Materialize the whole skill at activation:** rejected because the
  specification forbids fetching supporting files ahead of need.
- **A per-session cache directory:** rejected because parent, child and restarted
  root would see different paths.
- **A new mandatory sandbox entry:** rejected because process output already
  supplies the required daemon-written, session-read-only directory.
- **Upgrade mcp-go:** rejected because neither v1.5.0 nor v1.6.0 has skills methods.
- **TTL caching:** rejected because per-activation refresh already bounds
  staleness.
- **Load dynamic skills without verification:** rejected because it creates a
  second trust level for code the model may run.
- **Strict ADR-0089 visibility:** rejected because skills-only servers would be
  invisible everywhere.
- **Manager discovery from retained clients:** rejected because it would be
  complete for some roots and empty for others after every restart.
