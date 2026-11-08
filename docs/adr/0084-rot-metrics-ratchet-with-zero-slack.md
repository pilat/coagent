# ADR-0084: Rot metrics ratchet with zero slack

- **Status:** Accepted
- **Date:** 2026-11-08

## Context

`make arch` guards dependency direction and reachability, but nothing stopped the
numbers that measure rot from growing. At this decision `internal/tool/builtin` had
7218 production lines, `internal/sessionstore` 6467 and `internal/session` 5206;
`daemon` was allowed 30 of 55 components; production code held 127 `//nolint`
directives. `docs/coding-style.md` already sets a limit of 1000 lines per package,
and 17 packages exceed it. Enforced as written, that limit would demand refactors
before any other work. Left as prose, it stops nothing.

The tension is friction against drift. A gate strict enough to notice small growth
fires on ordinary feature work. A gate with room in it lets growth through in pieces
no single review would object to.

## Decision

`scripts/ratchet.sh`, run by `make arch`, compares these measures with the committed
`scripts/ratchet.values`: production Go lines per package, `mayDependOn` edges per
component and in total, the number of components allowed `anyProjectDeps`, and `//nolint`
directives in total.

`anyProjectDeps` is counted because flipping a component to it deletes that component's
edges, which would otherwise read as a shrink. Any `deps:` line the script cannot count
fails the gate for the same reason, so reformatting cannot hide an edge.

A value above its record fails, with zero slack: one extra line in one package fails
the gate. A value at or below its record passes, and a value below prints a one-line
hint. The gate never writes the file. `make ratchet.record` rewrites it from the
current tree, in the PR that deliberately moves a number, and that PR's description
says why.

## Consequences

- Growth becomes a line in the diff with a stated reason, instead of something a
  reviewer has to notice.
- Most feature PRs touch `scripts/ratchet.values`. That is the cost, and it is paid on
  purpose.
- Raising a value takes one command, so the ratchet does not prevent growth. It makes
  growth visible. The brake is the review that reads the values diff.
- Two PRs raising one package usually conflict in the values file and the second one
  re-records. Two can also pass alone and fail together without a conflict: both landing
  the identical value, or both spending headroom an unrecorded shrink left. `main` then
  sits above its record until the next PR re-records.
- A shrink that nobody records leaves headroom: that package can regrow silently up to
  its old record. Recording is a hint, not a requirement.
- Lines count comments and blanks. A long doc comment moves the number, and that is
  accepted for the sake of a unit that needs no parser.
- It measures only what it names. `.golangci.yml` thresholds and exclusion rules and
  `// nosemgrep` suppressions are outside it.

## Alternatives Considered

- **Slack per package (say 5%).** Each PR would spend the margin without saying so, and
  re-basing the record after a few PRs would restore the margin. The number would stop
  meaning anything.
- **Report without failing.** A number nobody has to act on gets read once. The repo
  already has one of these: the 1000-line limit in prose.
- **Enforce the 1000-line limit.** 17 packages fail today, so landing it would require
  the refactors this gate is meant to leave optional.
- **Fail until a shrink is recorded.** A refactoring PR would fail for doing the right
  thing, and only for a bookkeeping step.
- **Let the gate lower the record itself.** A gate that writes files dirties every local
  run and cannot commit in CI.
