# ADR-0067: Created worktrees inherit source-project escalation

- **Status:** Accepted
- **Date:** 2026-09-25

## Context

Project escalation is keyed by an exact canonical directory. `/gwt` creates a
separate project directory, so a session there loses developer-tool grants that
the operator assigned to its source repository. Requiring a second entry for
every branch makes short-lived worktrees need repeated configuration changes.
Some profiles expose credentials or host sockets, so inheritance must have an
explicit provenance boundary.

## Decision

A worktree created by the controller through `/gwt` inherits the source
repository's project escalation. A project entry for the worktree adds to that
set, and global escalation still applies to every project. The compiled policy
continues to remove all profile grants when session shields are raised.

The controller alone records the source repository root and a creation-origin
marker after creating the worktree. Manager-supplied values for either field
are refused; later attribute updates preserve them. Old sessions may contain
caller-supplied roots but have no controller-origin marker, so they retain
their previous Git-metadata access without inheriting project escalation.
Policy selection also checks the worktree namespace and Git common directory.
The Git check is only a consistency guard: the writable `.git` file cannot
prove provenance by itself. Ordinary nested projects and worktrees opened
without `/gwt` do not inherit.
Subagents read the value from their durable root session, so the same policy
selection survives child creation and restart.

## Consequences

- Granting `gh`, `ssh`, or another sensitive profile to a repository also
  grants it to `/gwt` worktrees from that repository. Operators can use shields
  to remove those profile grants for a session tree.
- Changing a project's escalation changes the effective policy of its
  worktrees after the configuration restart. Worktree-specific entries remain
  additive; they cannot remove inherited or global grants.
- The separate worktree directory retains its own project identity, temporary
  storage, and session lifecycle. Inheritance changes profile selection only.

## Alternatives Considered

- **Require an explicit entry for each worktree.** Rejected because `/gwt`
  worktrees are disposable branches of an operator-approved source project.
- **Inherit by filesystem ancestry or by any Git common directory.** Rejected
  because an arbitrary project or untrusted checkout could then select another
  project's credential grants.
