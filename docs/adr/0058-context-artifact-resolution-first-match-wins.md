# ADR-0058: context artifact resolution uses first-match-wins searches

- **Status:** Accepted
- **Date:** 2026-09-13

## Context

The loader that opens every session (`LoadAgentsMD`, `internal/loader/agents.go`) concatenated every file found in a flat list of five paths: the global `~/.coagent/AGENTS.md`, then the project's `AGENTS.md`, `CLAUDE.md`, `.claude/CLAUDE.md`, and `CLAUDE.local.md`. Three problems forced a decision:

1. **Alias double-load.** The agents.md standard itself recommends publishing one instruction document under several names via symlinks (`mv AGENTS.md && ln -s`). The coagent repo does exactly this (`AGENTS.md` → `CLAUDE.md`), and the flat-list loader read both names, loading the same file twice into every session and every subagent's opening context.
2. **Invisible globals.** The operator's global files are a symlink farm into one canonical directory: `~/.coagent/AGENTS.md`, `~/.claude/CLAUDE.md`, and `~/.codex/AGENTS.md` all point into the same place. The loader only read the coagent global, so the Claude Code / Codex / OpenCode / Gemini CLI globals were unreachable from coagent unless duplicated into `~/.coagent/AGENTS.md`.
3. **Symlink exfiltration.** With session shields down (the default), project-local sources (context artifacts, skills, subagents) were read through a host-readable access that resolved symlinks without containment — a malicious repo could ship `AGENTS.md` (or a skill or subagent file) as a symlink to a host file (for example `~/.coagent/secrets`) and have its content injected into the session's context or skill inventory.

The investigation also confirmed the subagent behavior the change interacts with: general and project-defined subagents already receive the same opening context as the parent (same `factory.Create` → `newWithOptions` path), while built-in `explore` skips it — the same split Claude Code documents for its Explore and Plan subagents.

The tension: concatenation loads everything that exists (alias duplicates, and a coagent global that silently shadows every other global); a strict single-file choice has to decide which of the many names in circulation (`AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `*.local.md`) is "the" file, and what happens to the personal local overlay.

## Decision

We resolve opening context as **three independent artifact searches — global, project, local — each first-non-empty-wins**, then merge what was found:

- **global** (≤1): `~/.coagent/AGENTS.md` → `~/.claude/CLAUDE.md` → `~/.codex/AGENTS.md` → `~/.config/opencode/AGENTS.md` → `~/.gemini/GEMINI.md`
- **project** (≤1): `<workDir>/AGENTS.md` → `<workDir>/CLAUDE.md` → `<workDir>/.claude/CLAUDE.md`
- **local** (≤1, appended as an overlay, not an alternative): `<workDir>/AGENTS.local.md` → `<workDir>/CLAUDE.local.md`

The merged string renders each found artifact as a `[role] <path>` labeled section (global, project, local) with the home directory abbreviated to `~`; the transcript marker `agentsMDMessagePrefix` stays byte-identical. A missing file, broken symlink, or whitespace-only candidate is skipped; any other read error records the candidate and continues the search — a candidate failure degrades its own search, it never blanks the other artifacts (`LoadAgentsMD` keeps its `(string, error)` contract, returning the partial merge with the joined errors).

All project-local sources (context artifacts, skills, subagents) are always read through a project-confined root, and marketplace sources through a root confined to their own repository clone — an escaping symlink is denied and never loaded even when session shields are down; global sources remain trusted daemon inputs read from the host.

## Consequences

- At most three instruction files per session (usually one) instead of up to five concatenated: no alias double-load, and the operator's canonical global is reachable through any of the five names.
- The session can see which file won: each section carries a role label and the winning candidate path (home abbreviated), which also keeps the username out of the rendered context.
- Repos where `AGENTS.md` and `CLAUDE.md` are two different documents now see only `AGENTS.md`. That is the price of first-match-wins and matches OpenCode's documented precedence ("The first matching file wins in each category").
- Coagent supports the emerging `AGENTS.local.md` convention ahead of any shipping tool (openai/codex#26957, anomalyco/opencode#16110, and agentsmd/agents.md#72 are all open as of this date).
- A read failure on one candidate (permissions, a directory at the path, a shield denial) degrades that search instead of blanking the whole opening context; the denial is still surfaced via the returned error and the caller's warning, and the denied content never enters the context.
- A malicious repo — project or marketplace — cannot exfiltrate host files (for example `~/.coagent/secrets`) through a symlinked instruction file, skill, or subagent: with or without session shields, an escaping symlink is denied and never loaded. For context artifacts the denial is additionally recorded in the returned error; for skills and subagents an unreadable entry is skipped, while a broken source directory is still recorded.
- Cross-version edge, accepted: a `context_reset` delivery claimed under an older binary and redelivered under the new one will surface `ErrDeliveryConflict` instead of deduping silently (the delivery fingerprint is derived from the merged string, `internal/session/session.go:433`). The window is narrow — claim and effect commit atomically.
- Follow-up work created: walk-up / nested `AGENTS.md` (monorepo) support remains out of scope and is a separate feature.

## Alternatives Considered

- **Concatenate all existing files** (the status quo): rejected. It duplicates one artifact under alias names (the coagent-repo bug), and a created `~/.coagent/AGENTS.md` would silently shadow the operator's other globals in the opposite direction of the intended fallback.
- **A single flat chain across all ten paths**: rejected. Scopes must not be alternatives to each other — a global file must never be an alternative to a project file.
- **Dedup by resolved real path (`EvalSymlinks`)**: rejected. Unnecessary once the search stops at the first non-empty candidate; the alias double-load disappears without path resolution.
- **Reading `~/.codex/AGENTS.override.md`**: rejected. It is Codex's temporary-override mechanism; loading it from coagent would be surprising.
- **A Plan subagent that receives no project context** (Claude Code's other context-skipping built-in): rejected. Coagent planning is a warm-context skill flow (brainstormer → plan-handoff); a cold Plan subagent would lose the conversation's decisions.
- **A marketplace containment root at the cache base** (`~/.coagent/cache/marketplaces`): rejected in favor of a per-repository root. Each marketplace clone is independent untrusted input; a cache-base root would let one repo's plugin symlink into a sibling repo's content, while a per-repo root denies that and still allows the legitimate in-repo symlink pattern.
