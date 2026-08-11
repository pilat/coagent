# ADR-0017: A bound control socket answers, and "starting" is a state clients can read

- **Status:** Accepted
- **Date:** 2026-08-17

## Context

`connect(2)` succeeding on `~/.coagent/daemon.sock` is the daemon's liveness
test — there is no ping op. But binding and answering were separated by the whole
boot: `NewServer` bound the socket, and `Serve` ran only after the local chat and
every configured manager had started. A Telegram manager spends four blocking
HTTP round trips there.

In that window a client connects successfully into the kernel backlog and then
waits for a greeting that nobody writes. After three seconds `Dial` returned a
bare `read greeting: i/o timeout` — not `ErrNotRunning`, not anything a caller
could classify. `coagent status` exited 1 ("could not ask") and bare `coagent`
aborted, both against a perfectly healthy daemon that was still coming up. The
socket was documented as a "truthful readiness boundary", but binding is what
makes connect succeed, so the boundary was never actually there.

Serving from the bind has its own hazard: ops are registered by their owners
during that same window, so a client could reach a half-built registry and get
`unknown method` for an op that exists a moment later.

## Decision

The daemon accepts connections from the moment the socket is bound, and reports
the boot as its own state.

- `Server.ServeStarting(ctx)` starts the accept loop; `MarkReady()` ends the
  starting phase. `Serve` is `MarkReady` plus the loop, for embedders that are
  ready at construction.
- While starting, **every** op — `status` included — is refused with a single
  JSON-RPC code `-32000`. The handler map is never consulted, so a partially
  registered registry cannot leak as `unknown method`. `status` is refused rather
  than answered because the managers it would report have not been started yet,
  and "running: false" for a manager that is about to come up is a lie.
- Registration closes at `MarkReady` instead of at the first accept, which is
  what makes the phase usable.
- Clients get one sentinel, `ctl.ErrStarting`: from `-32000`, and also from a
  greeting that does not arrive within the dial budget — an older daemon binds
  long before it serves, and "bound but silent" means the same thing.
- Bare `coagent` waits it out with the poll it already uses for a fresh install.
  `coagent status` prints `daemon starting — not answering yet` and exits **2**,
  the retryable code, rather than 1.

## Consequences

- A daemon is never reported as broken for being slow to boot, and a supervisor
  script sees a retryable code instead of a hard failure.
- Exit code 2 now covers two states ("no daemon" and "not ready"). The printed
  line distinguishes them; the exit contract stays three-valued on purpose,
  because both states have the same answer: wait and ask again.
- A daemon that wedges during startup reads as "starting" forever instead of as a
  timeout. That is honest — from outside, a wedged boot and a slow boot are the
  same observation — but it means "starting" is not by itself proof of progress.
- Every embedder of `ctl.Server` must reach a ready state. A server that only
  ever accepts answers `-32000` to everything; the failure is immediate and
  loud rather than silent.

## Alternatives Considered

- **Keep the bind→serve gap and only classify the greeting timeout as
  `ErrStarting`.** Smaller change, and it is kept as a fallback for older
  daemons, but every probe costs the full dial budget, and it leaves the socket
  connectable-but-mute — the exact property that made the state unreadable.
- **Answer `status` during the starting phase.** Tempting for diagnostics, but
  its manager section would be fiction until the managers have started, and the
  fields it does know (pid, uptime, config path) do not justify a status result
  that lies about the part callers read it for.
- **Delay the bind until everything is up.** Then the boot window looks like "no
  daemon", and bare `coagent` would offer to install a daemon that is already
  running.
