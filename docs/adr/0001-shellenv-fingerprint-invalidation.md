# ADR-0001: Fingerprint-based invalidation for shell-env snapshots

- **Status:** Accepted
- **Date:** 2026-08-02

## Context

Tool subprocesses (bash commands, MCP servers, LSP servers) must run under the
per-directory activated toolchain (mise/asdf/nvm/direnv PATH, env vars, shell
functions). The daemon captures this once per workdir by running a login+interactive
bash and caching a re-sourceable snapshot, then wraps every spawn as
`bash -c "source <snapshot>; exec <cmd>"`.

Capture costs a login+interactive shell (~tens–hundreds of ms), so it must be cached.
The snapshot was cached by workdir with a 5-minute wall-clock TTL. That TTL was doing
invalidation's job, badly: when the environment actually changed — most importantly
when the **agent itself** ran `mise install`/`mise trust`/`mise use`, edited
`mise.toml`/`.envrc`, or installed a global tool — subsequent spawns kept using the
stale snapshot for up to 5 minutes, including the case where the agent had just fixed
its own broken toolchain.

The tension: the cache's primary consumer is bash, which is also the primary source of
env mutation. So "drop the snapshot after every bash" is equivalent to no cache, and
"parse each command to guess whether it mutated the env" is an unbounded, unreliable
classifier (false negatives leave staleness; false positives waste captures).

## Decision

We treat the snapshot as a build artifact whose prerequisites are files on disk, and
validate it against those prerequisites — not against wall-clock time.

- The activated env is a pure function of on-disk state. We compute a **fingerprint**
  over the *controlled set*: the walk-up config chain from workdir to `$HOME`
  (`mise.toml`, `.mise.toml`, `.tool-versions`, `.nvmrc`, `.node-version`,
  `.python-version`, `.ruby-version`, `.envrc`), global mise config, the rc files, and
  manager state whose *own* mtime moves on change — the mise trust store, the direnv
  allow db, and the nvm node dir (each gets a direct child on trust/install). Install
  dirs laid out as `<dir>/<tool>/<version>` (mise/asdf) are **scanned one level deep**:
  a new version of an already-installed tool bumps the `<tool>` subdir's mtime, not the
  `installs/` dir's, so stat'ing only the top dir would miss it (verified empirically).
  Each entry contributes `(path, exists, mtime, size)`, plus a content hash for the small
  regular config files. **Negative entries are load-bearing:** a config appearing where
  none existed is a change. A snapshot is reused only when its fingerprint still matches;
  otherwise it is recaptured. This catches agent-initiated *and* out-of-band changes.
- The **wall-clock TTL is demoted to a 30-minute backstop** for the un-fingerprintable
  residue (an rc file sourcing something we don't track).
- The **negative cache of failed MCP servers is tied to the same fingerprint**: a
  failure records the workdir's fingerprint, and a fingerprint change invalidates it —
  so a server broken by a toolchain problem the agent just fixed is retried on the next
  spawn, not after a fixed cooldown. A short TTL remains as a secondary backstop.
- A **regex over completed bash commands** (`\b(mise|asdf|nvm|direnv|conda)\b|npm .*-g`)
  is a planned *background pre-warmer* — **not yet implemented** (deferred follow-up). It
  would call `Invalidate` to force a recapture, but never owns correctness: a false positive
  costs one hidden recapture, a false negative costs nothing because the fingerprint is
  authoritative. `Provider.Invalidate` exists for it; until it ships, `Invalidate` has no
  production caller and the fingerprint alone carries invalidation.
- We do **not** harvest managers' own watch lists (`__MISE_WATCH`, `DIRENV_WATCHES`) as
  the primary signal: they are absent from our snapshots' `export -p`. Harvested if
  present, treated as a bonus, never a dependency.
- On capture failure we **degrade to no-snapshot** (spawn with the inherited env), never
  reuse a "last known good" snapshot — a silently wrong env is worse than an honestly
  degraded one.

## Consequences

- File-driven env changes (nearly all of them) are reflected on the **next spawn**, not
  after a wall-clock delay. The headline case — agent fixes its toolchain, its tools see
  it immediately — works.
- The hot path pays ~20 `stat`s per spawn (microseconds through the dentry cache) plus a
  small content hash of the workdir configs. Negligible next to a subprocess spawn.
- The daemon recaptures once per workdir after a restart (the in-memory fingerprint map
  starts empty). Acceptable.
- The `Provider` interface gains `Fingerprint` and `Invalidate`; mocks must implement
  them.
- `Invalidate` currently has **no production caller** — its intended caller, the regex
  pre-warmer, is a deferred follow-up. The fingerprint's state-dir depth handling already
  covers agent-initiated `mise install`/`trust`/`use` and `nvm install`, so the headline
  promise holds without it; the pre-warmer would only *accelerate* a recapture, not enable it.

## Alternatives Considered

- **Invalidate after every bash command** — equivalent to disabling the cache, since
  bash is what the cache serves. Rejected.
- **Classify commands as env-mutating or not** — unbounded surface (mise/asdf/nvm/direnv/
  pyenv/conda/exports/global installs/rc edits/`eval`/wrappers), unreliable both ways.
  Kept only as the non-authoritative pre-warmer.
- **inotify/FSEvents watchers** — the cache is read only at spawn time, so a per-spawn
  `stat` is exactly as fresh as anything a watcher could provide; watchers add lost-watch-
  on-rename, fanout, and platform divergence for no benefit. Rejected.
- **Persistent per-workdir shell servers** — env always live, but trades this problem for
  state bleed between commands, hangs wedging the session, and a pool/recovery lifecycle.
  Per-spawn processes stay isolated and timeout-killable. Rejected.
- **`mise env --json` per spawn** — loses bashrc functions, aliases, and nvm (a shell
  function). Rejected.
- **Harvest `__MISE_WATCH`/`DIRENV_WATCHES` as the source of truth** — not present in our
  captures. Bonus only.
- **Reuse the last-good snapshot on capture failure** — unreliable; a stale env that looks
  valid causes silent, hard-to-trace wrong behavior. Rejected.
