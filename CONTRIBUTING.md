# Contributing to coagent

Thank you for helping improve coagent. Before changing code, read the project
vocabulary in `docs/glossary.md`, the architecture contract in
`ARCHITECTURE.md`, and `docs/coding-style.md`.

## Set up the checkout

Install the versions pinned by `mise.toml`, then bootstrap dependencies and
development tools:

```bash
mise install
make tools
make verify-offline
```

`make tools` is the only online bootstrap target. Normal verification fails
closed when a pinned tool or cached dependency is missing.

## Make a change

- Keep changes focused and add deterministic tests for observable behavior.
- For lifecycle, durable state, concurrency, retry, restart, queues or
  cross-package events, follow the protocol-testing levels in `docs/testing.md`.
- Record significant, hard-to-reverse decisions as the next ADR in `docs/adr/`.
- Update `ARCHITECTURE.md` only for stable ownership, protocol or trust-boundary
  changes; obey its line budget and maintenance contract.
- Never use real credentials, user state or mutable network repositories in
  tests.

Apply formatting with `make fmt`, then run `make all`. The local test target
omits complete lifecycle, migration, protocol-store, manager and external-process
package suites; use an exact `-run` selection when changing one of them.
Integration, E2E, fuzz, race, stress and mutation targets are CI/CD-owned and
reject local execution. Architecture checks run locally in `make all`; Semgrep
and secret-history gates are CI/CD-owned. Do not set `CI=true` locally to bypass
that boundary.

## Open a pull request

Explain the problem, the chosen behavior, tests run and any security or
compatibility impact. Link an issue when one exists. By contributing, you agree
that your contribution is licensed under the repository's `LICENSE`.
