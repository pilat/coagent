# ADR-0109: Status is a coagent.v1 call on the control socket

- **Status:** Accepted
- **Date:** 2026-11-24
- **Amends:** [ADR-0017](0017-a-bound-control-socket-answers.md)
- **Amends:** [ADR-0104](0104-the-local-api-is-the-two-durable-ledgers.md)

## Context

ADR-0104 kept the status-only `daemon.sock` on newline-delimited JSON-RPC with a greeting while adding
`coagent.v1` on `api.sock`. The daemon carried two protocols for one read: custom framing, greeting, dispatch,
client and connection bookkeeping beside the generated ConnectRPC contract. Status needs neither a second
protocol nor the authenticated author and session capabilities served on the API socket.

`coagent status` must keep working without a configuration file or token. A sandboxed session can connect to
either same-user socket through its read-only host view, so the API token boundary must remain intact.
ADR-0017 also requires a bound socket to answer immediately and distinguish a starting daemon from an absent
one; reporting managers before startup would falsely describe them as down.

## Decision

One `localapi.Server` owns two `connect.Server`/`http.Server` pairs, one per socket:

- `~/.coagent/daemon.sock` is always bound with mode `0600`. Its anonymous registration contains only
  `coagent.v1.DaemonService.GetStatus`. Every other procedure returns HTTP 404 / `Unimplemented`, including
  during startup. Registration is complete at construction, so an unknown procedure is truly unknown.
- `~/.coagent/api.sock` is bound only when `api.clients` is non-empty. It keeps the same four services:
  `SessionService`, `OutputService`, `ChatService` and `DiscoveryService`. Token middleware authenticates every
  request, with no exemption. The in-process connection keeps those same four services and does not expose
  `GetStatus`; programs read status from `daemon.sock` without a token.

The composition root holds the single-instance flock; only its holder may remove stale sockets and bind.
The local API server owns both binds and readiness. Failure to bind the control socket remains fatal, while
failure to bind or attach the optional API socket is logged and isolated.
Stopping closes status connections at once and drains only `api.sock` within `closeGrace`.

An atomic readiness flag makes `GetStatus` return `Unavailable` with `daemon is starting` until the managers
have started. After readiness, the projection preserves providers, models, stale models, search and manager
liveness. Manager startup errors arrive credential-redacted from their runtime. The response keeps `boot_id`
as the identity of this daemon run. The greeting and its `protocol_version` disappear; the versioned package
defines the wire contract, and `binary_version` already names the binary.

The CLI bounds the whole call, including its Unix dial, by three seconds. It classifies failures by successful
connection, remote error code and context cause, never by matching error text:

- No connection: print `daemon not running` and exit 2.
- A remote `Unavailable`, or a deadline after connecting: print `daemon starting — not answering yet` and exit 2.
- Any other failure after connecting: print the error and the fixed hint below to stderr, then exit 1.

```text
the daemon may be running an older coagent binary — `sudo coagent restart` to apply this one
```

Successful output, including version skew, keeps its existing bytes and exit code 0. No old protocol client
is retained.

## Consequences

- `internal/ctl` is deleted. `coagent.v1` supplies the sole public protocol while the two sockets keep their
  distinct authentication boundaries. A sandboxed session can still read status and cannot reach any other
  call through `daemon.sock`.
- ADR-0017's liveness guarantees remain: a refused connection means absent; remote `Unavailable` means
  starting; a bound socket silent for three seconds also means starting. Unknown procedures now return
  `Unimplemented` while starting because registration no longer changes during boot.
- A new CLI against an old daemon prints the transport error and older-binary hint and exits 1, instead of
  reading status successfully. Restarting onto the new binary ends this upgrade window. An old CLI against a
  new daemon waits three seconds for its greeting, reports starting and exits 2.
- Against an old daemon, net/http may log `Unsolicited response received on idle HTTP channel` and Connect may
  prefix its error with `possible h2c configuration issue`; this one-time noise does not change the exit or hint.
- A daemon hanging inside `GetStatus` after readiness reads as starting after three seconds; the previous
  client could wait indefinitely after its greeting. A reset during restart exec yields exit 1 and the hint;
  rerunning status reads the new process.
- The daemon no longer logs every status request. Status carries no greeting or `protocol_version`, and
  `GetStatus` is unavailable on the API socket and in-process connection.

## Alternatives Considered

- **One socket with an authentication exemption.** Saves about 25 lines but introduces a hole in the token
  middleware, binds `api.sock` on every installation, and makes a new CLI falsely report an old daemon without
  API clients as absent.
- **Keep the old client for one release.** Retains about 380 lines and a second protocol for a single upgrade
  window. An explicit restart hint provides a recovery path without carrying compatibility machinery.
- **Recognize an old daemon by parsing the HTTP error.** Couples classification to library error wording.
  Successful connection and structured errors provide the required distinction without text matches.
