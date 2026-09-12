# ADR-0054: Background capacity does not block foreground Bash

- **Status:** Accepted
- **Date:** 2026-09-11

## Context

The background-process service starts every Bash command under the same durable
supervisor so a command that exceeds the foreground grace can be promoted
without changing process ownership. Its original admission counter therefore
counts both advertised background jobs and short, unadvertised foreground
candidates against four per-session slots.

When four long jobs are running, the shared counter rejects every new Bash call
before launch. The model cannot run a short diagnostic, inspect a reported
process output file, or execute unrelated foreground work. Error guidance then
asks the model to cancel work or wait, exposing an internal capacity constraint
as a reasoning problem.

Foreground candidates still need bounded ownership: they have output limits,
deadlines, guardians, stop/kill behavior, and a possible transition into
background work. The design must preserve those lifecycle guarantees without
queueing commands whose preconditions may become stale.

## Decision

Process admission has two per-session classes. Up to four advertised background
processes may run, and one additional unadvertised foreground candidate may run.
The service retains total-live accounting for shutdown and records each tracked
process's class in memory.

A process launched with `Spec.Advertise=true` reserves background capacity
before launch. Every other process reserves the candidate class. The existing
sub-ten-second deadline rule still makes a `background=true` command a
foreground-only candidate when it cannot outlive the foreground grace.

At the grace boundary, service-level advertisement holds the lifecycle mutex,
checks background capacity, performs the durable running-to-advertised CAS, and
then moves the in-memory class. Store failure or a losing CAS leaves the class
unchanged. Terminalization uses the same mutex and releases the recorded class
exactly once.

When all background slots are occupied, automatic promotion returns a capacity
result without changing the candidate. Bash continues waiting for the ordinary
terminal outcome under the original deadline and supervisor. The candidate is
never advertised and emits no asynchronous completion. Explicit advertised
launch still fails before starting when background capacity is full.

## Consequences

- Four background jobs no longer prevent one serial foreground Bash operation.
- A long foreground command that cannot promote retains a session runner slot
  until it exits or reaches its deadline.
- Background capacity remains fixed and visible; the daemon neither evicts work
  nor starts it later from a hidden queue.
- Process cancellation, tree stop/kill, output overflow, deadline, guardian,
  startup interruption, and shutdown join both admission classes.
- Admission accounting becomes class-aware and must cover launch rollback,
  durable insert failure, promotion, terminalization, and cancellation races.

## Alternatives Considered

- **Increase the single limit.** Rejected because foreground traffic could fill
  the larger pool and still starve diagnostics; the two workloads have different
  semantics.
- **Run foreground Bash outside the process service.** Rejected because an
  already-running command could not be promoted while retaining its guardian,
  output artifact, deadline, and stop/kill ownership.
- **Queue commands after the fourth process.** Rejected because delayed shell
  execution can observe stale files and state and requires durable deadline,
  cancellation, restart, and deduplication rules.
- **Kill the candidate when promotion is full.** Rejected because it may already
  have performed side effects and the caller asked for foreground execution.
- **Retry promotion when capacity frees.** Rejected because it adds a scheduler
  and another transition race; the bounded foreground wait is sufficient.
- **Automatically cancel an existing background process.** Rejected because the
  daemon cannot infer which work is redundant or safe to discard.
