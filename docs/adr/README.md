# Architecture Decision Records

An ADR captures **one** significant, hard-to-reverse decision and the reasoning behind it — the *why* that `ARCHITECTURE.md` deliberately leaves out. ARCHITECTURE.md describes the system as it is now (and is rewritten as the system changes); an ADR is a dated snapshot of a choice and stays frozen even after the code moves on. Six months later, "why is it done this way and not the obvious way?" is answered here, not by reading the diff.

## When to write one

Write an ADR when a decision is **significant and hard to reverse** — a tradeoff someone might reasonably question later:

- Picking one technology/library/protocol over a viable alternative (e.g. goose over hand-rolled migrations, SQLite over an embedded KV).
- A cross-cutting invariant that new code must respect (e.g. append-only context log, secrets never entering the process environment).
- A boundary or layering rule that constrains where logic may live.
- Deliberately rejecting the obvious approach for a non-obvious reason.

Do **not** write one for routine, easily-reversed choices (a helper's name, a local refactor, a bug fix). If reversing it later is cheap and uncontroversial, it isn't an ADR.

The trigger lives in `CLAUDE.md` (Decision Records) so it fires while you're making the decision — by the time you open this file you're already writing one.

## Filename convention

`docs/adr/NNNN-kebab-case-title.md` — a zero-padded, sequential number and a short slug:

- `0001-goose-for-migrations.md`
- `0002-append-only-context-log.md`

The number is monotonic (next free integer, never reused) so ADRs sort chronologically and are easy to reference in prose ("see ADR-2"). Copy `TEMPLATE.md` to start.

## Immutability

An accepted ADR is **immutable**. You don't edit a decision to reflect a new one — you write a new ADR that supersedes it and link back:

- Set the old ADR's status to `Superseded by ADR-NNNN`.
- The new ADR's Context explains what changed since the original.

This preserves the historical record: the old reasoning was correct given what was known then, and that context is worth keeping.
