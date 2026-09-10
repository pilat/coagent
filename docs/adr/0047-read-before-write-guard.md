# ADR-0047: Guard blind overwrites with a durable per-session read ledger

- **Status:** Accepted
- **Date:** 2026-09-08

## Context

Coagent's `write` tool overwrites a file wholesale with no check that the model
ever saw the prior content, or that the content is still what the model last
read. Its description has claimed "This tool will fail if you did not read the
file first" since the first commit, but nothing enforced it. An unattended agent
can therefore clobber a file it never opened, or overwrite a version a linter,
`git`, or the human changed under it — silently losing work.

`edit` and `apply_patch` do not share this exposure once `apply_patch` is text
-anchored (ADR-0048): `edit` requires an exact `old_string` and `apply_patch`
matches its context, so both fail cleanly rather than corrupt when a file moved.
`write` is the only mutator with no text anchor.

Every comparable tool that added this guard (Claude Code, OpenCode) tracks reads
with a timestamp and rejects a stale mutation. Two things repeatedly bite them:
in-memory tracking loses state across restart (coagent is a restart-surviving
daemon), and bare mtime produces false "modified since read" rejections from
idempotent formatters, `touch`, and undo — their single most-reported bug.

## Decision

We add a durable per-`(session, path)` ledger table `session_file_reads` storing
`{mtime_unix_nano, size, hash}`. `read` records an entry; `edit`, `apply_patch`,
and `write` refresh it after a successful mutation. **Only `write` checks it**,
and only before overwriting an existing file:

1. an entry must exist (the file was read or edited in this session);
2. the file must be unchanged — mtime match is the fast path; on mtime mismatch
   we compare the content hash, so identical bytes with a bumped mtime pass and
   only real changes reject.

A `write` that creates a new file skips both checks. `edit` and `apply_patch`
never reject on the ledger. The ledger key is a single canonical path computed
identically at record and lookup. mtime is stored as an integer, never SQLite
`DATETIME`.

## Consequences

- A blind or stale `write` to an existing file is refused with a message telling
  the model to read (or re-read) it first; the guard survives daemon restart, and
  detects files changed while the daemon was down.
- The hot `edit` path keeps zero ledger friction — the linter/undo false-positive
  class that plagues uniform guards never touches it.
- New durable protocol: a migration, a store capability, and a session-scoped
  shim (mirroring `todoReplacement`). `edit`/`apply_patch` must refresh the ledger
  even though they never read it, or a following `write` falsely rejects.
- `write`'s authorize runs inside the existing per-path write lock; the hash
  fallback re-reads the existing file only on the mtime-mismatch path.
- Mutations via `bash` bypass the ledger by design; the next `write` correctly
  sees the mismatch. Ledger rows are not swept per session (bounded, like
  `session_tool_activations`).

## Alternatives Considered

- **Reuse `session_tool_activations`.** Its schema forbids it: a unique partial
  index caps one live row per session, PK is `input_id`, `command` must start with
  `/`, there are no path/mtime columns, and the lifecycle is consume-once.
- **Persist read state in the transcript/chat.** No authoritative chat state
  exists — `tool.Result.Metadata` is runtime-only and compaction would drop any
  marker embedded in message content.
- **A JSON blob on the `sessions` row** (like `todo_items`). Rewrites the whole
  blob on every read and races root vs. subagent; a keyed child table is the
  established pattern for per-item durable state.
- **Guard all three mutators.** Rejected once `apply_patch` is text-anchored:
  guarding the self-protecting `edit`/`apply_patch` only adds the false-positive
  friction the leaders suffer.
- **Bare mtime, no hash.** Rejected — the dominant real-world false-positive
  source for every guard that relies on it.
