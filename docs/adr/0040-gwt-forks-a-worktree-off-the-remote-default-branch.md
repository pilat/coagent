# ADR-0040: /gwt forks a worktree off the remote default branch

- **Status:** Accepted
- **Date:** 2026-09-02

## Context

Telegram sessions run in a project directory that is often a git repository.
Operators want to spin off a parallel line of work — a fresh branch and working
tree — without disturbing the session they are in, whose directory may hold
uncommitted junk. The prior mechanism (a "🌿 GWT" spawn button setting
`SessionCreateData.UseWorktree`) cut a timestamped branch off the current `HEAD`
into a sibling directory, retried silently on a name clash, and offered no way
for a repository to prepare the new tree.

Several forces pull on the design. The tool ships in an open-source binary that
must work on Linux and macOS under any user's git and credential configuration —
nothing may depend on one maintainer's machine. The new tree must be reproducible
from a known-good base, not from whatever the operator happened to have checked
out. And per-repository setup (env files, dependency installs, toolchain trust)
belongs to the repository, not to coagent.

## Decision

We add `/gwt <name>`, a session-topic command that forks the current session's
repository into a new git worktree and opens a new session on it, leaving the
current session untouched.

- **Base is the remote default branch, fetched fresh.** We read the default
  branch authoritatively from the remote (`git ls-remote --symref <remote> HEAD`,
  so a non-`main` default is honored), `git fetch` it, and branch off
  `<remote>/<default>` with `--no-track`. Branching never derives from the
  invoking directory's `HEAD` or working state, so local junk is neither touched
  nor inherited. A repository with no remote is refused — `/gwt` is remote-first
  and does not guess.
- **Repository setup is the repository's, via native git hooks.** `git worktree
  add` runs the repository's `post-checkout` hook in the new tree; coagent
  suppresses no hooks. There is no coagent-specific prep contract.
- **The name is taken verbatim.** It is validated (`projectpath.SanitizeName`, a
  leading-dash guard, `git check-ref-format`) but never decorated. A taken branch
  or existing path is refused rather than renamed.
- **Failure is atomic.** Any non-zero `git worktree add` (including a failing
  `post-checkout` hook, whose exit status git propagates) refuses the session,
  surfaces git's combined output, and rolls back a worktree that was already
  materialized, so the command is re-runnable under the same name.
- **Worktrees live outside the projects root.** They are placed at
  `<worktrees_root>/<repoBasename>-<hash(repoRoot)>/<name>` (default
  `~/.coagent/worktrees`, auto-created; configurable via `worktrees_root`). The
  per-repo hash separates same-named repositories and keeps every worktree at
  least two levels below the root, so it is never a direct child of the `/new`
  picker root and never pollutes that picker.
- **The project reads as `<repo>/<branch>`.** The worktree's project is
  registered under an explicit display name rather than derived from the
  directory basename, so forks sharing a branch name across repositories stay
  distinguishable in session lists and Telegram topics. This mirrors the
  `sys:coagent` system project, whose name already differs from its
  `sys_coagent` directory. The name is display-only and not path-safe; the only
  place that reconstructs a path from a project name — the `/new` picker —
  already excludes worktree projects.

## Consequences

- Forking is a first-class, scriptable command; the GWT spawn button and the
  `UseWorktree` flag are removed.
- New branches start from the fresh remote default regardless of the operator's
  local drift, at the cost of a network round-trip on every `/gwt`. Offline use
  and repositories without a remote are unsupported by design.
- Repositories express setup through ordinary git hooks they already understand;
  coagent gains no bespoke hook format to document or version.
- Worktree projects are excluded from `/new` structurally (by path layout), so no
  project-registry flag or migration is needed to hide them.
- Cleanup is not automated: `/kill` still ends only the chat, and worktrees
  accumulate on disk until removed by hand.

## Alternatives Considered

- **Branch off local state (no fetch).** Offline-safe and never blocks on
  credentials, but bases work on possibly-stale or drifted local refs. Rejected:
  the point of a fork is a clean, current starting line.
- **A coagent-defined prep hook (e.g. `.coagent/hooks/post-worktree`).** Gives
  coagent an exit code to gate on, but invents a second hook system beside git's.
  Rejected: `git worktree add` already runs `post-checkout` and propagates its
  failure, so native hooks cover the need.
- **A project-registry `is_worktree` column to hide forks from `/new`.** Explicit,
  but needs a migration and store plumbing. Rejected: the picker already lists
  only direct children of the projects root, so a nested worktree layout excludes
  forks for free.
- **Decorate clashing names with a suffix or timestamp.** Convenient, but yields
  branch and directory names the operator did not choose. Rejected: refusing a
  taken name is more predictable.
