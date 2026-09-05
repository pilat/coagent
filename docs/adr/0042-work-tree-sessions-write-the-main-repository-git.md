# ADR-0042: Work tree sessions write the main repository's .git

- **Status:** Accepted
- **Date:** 2026-09-05

## Context

`/gwt` sessions run in a linked git work tree. The Bash sandbox confines writes
to a writable-roots list, so git operations in such a session need sandbox
access to git state that lives outside the work tree. A linked work tree shares
almost everything with the main repository: the object store, refs, config,
hooks, and reflogs live in the main `.`; only `index`, `HEAD`, and per-work-tree
admin files live under `<main>/.git/worktrees/<name>`.

An earlier attempt (#52) mounted the main `.git` read-only and punched a
writable hole for the per-work-tree admin directory. It failed in practice:
every mutating git command — starting with `git add`, which writes blobs —
needs the shared object store and shared refs. Sessions could not commit and
invented workarounds such as cloning to `/tmp` and pushing from there. The
read-only mount was additionally never honored by the macOS Seatbelt backend,
and the writable hole depended on a fragile guess that the work tree's
directory basename equals git's admin-directory name.

## Decision

For work tree sessions, `RepoRoot/.git` is added wholesale to the Bash sandbox
writable roots. The read-only-paths mount mechanism and the basename-derived
hole are removed.

The sandbox is confinement against accidental damage, not a hostile-model
boundary. Git itself gives any work tree write access to the shared metadata —
the sandbox can only grant or break what git requires, and a path-carved
middle ground cannot enumerate git's writes (gc, commit-graph, packed-refs,
quarantine). The trust granted is exactly the trust a linked work tree implies.
The main work tree's checkout files remain read-only.

## Consequences

- Git fully works in `/gwt` sessions: commit, fetch, merge, tag, stash, gc.
- A work tree session can move or delete main-repository refs, up to the
  authority of the credentials git itself uses — no stronger and no weaker
  than before.
- The sandbox backends no longer carry a read-only-roots concept; the Linux
  and macOS backends treat writable roots identically again.
- A repository whose git dir is not `<repoRoot>/.git` (separate-git-dir
  layouts) still gets no git access; `RepoRoot` is resolved only for standard
  layouts. Sessions over such repositories fail sandbox normalization loudly
  rather than silently mis-committing.

## Alternatives Considered

- **Read-only `.git` with writable holes for objects/refs/logs.** Preserves
  the "main repository is protected" story, but git's write surface is not
  enumerable, so every uncommon operation eventually breaks. Rejected — this
  is #52's failure mode with more holes.
- **Keep `.git` read-only; sessions commit via a `/tmp` clone.** Moves the
  workaround into the product: every session pays a clone, and the work tree's
  index, hooks, and state diverge from what is pushed. Rejected.
- **Per-work-tree object store (GIT_OBJECT_DIRECTORY / alternates).** Would
  make only the work tree's own objects writable, but invents a repository
  topology git users do not recognize and complicates push/fetch semantics.
  Rejected.
