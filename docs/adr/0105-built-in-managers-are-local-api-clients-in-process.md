# ADR-0105: Built-in managers are local API clients in process

- **Status:** Accepted
- **Date:** 2026-11-23
- **Amends:** [ADR-0104](0104-the-local-api-is-the-two-durable-ledgers.md)
- **Amends:** [ADR-0097](0097-orchestrator-mode-is-a-human-granted-capability-on-an-ordinary-root.md)
- **Amended by:** [ADR-0106](0106-one-operator-every-surface-reaches-every-root.md) — owned-root reads cover every root

## Context

[ADR-0104](0104-the-local-api-is-the-two-durable-ledgers.md) made `coagent.v1` the daemon's public contract, but
Telegram, the only production chat surface, still held the private manager-bound controller. The core therefore had
two front doors, and nothing showed that the public one was enough to write a chat surface: a1 served three RPCs
covering two of the 25 controller methods Telegram calls.

The controller returns refusals as error text that Telegram shows verbatim. Telegram matches no controller error by
type, so text is the whole error contract. connect-go v2 ships an in-process transport that dispatches to a
`connect.Server` with no network, socket or HTTP encoding.

## Decision

- Telegram is written against the generated `coagent.v1` clients and reaches the core only through them. The
  composition root gives each configured manager a transport from `localapi.InProcess`. Behind it is a
  `connect.Server` whose interceptor stamps the configured manager id as the author of every call. There is no
  socket and no token, and nothing on the API socket can reach this server. The handlers are the same values the
  socket serves, registered through one function.
- `coagent.v1` grows by `ChatService` (focus and cursor), `DiscoveryService`, and new `SessionService` and
  `OutputService` RPCs. Each RPC calls exactly one controller method. A request carries only fields the server acts
  on, so a `chat_id` appears only on the chat focus and cursor calls. A response carries only what a chat surface
  reads.
- New handlers validate nothing but page limits (1-100). Every refusal is the controller's text as a
  `FailedPrecondition` message. Telegram's client interceptor turns a connect error back into its message, so every
  reply is byte-identical.
- `StreamSessionEvents` carries wake hints only: a root id and activity or heartbeat, never content. Its first event, with
  session id 0, marks the subscription live. Telegram waits for it before delivery starts, which keeps the order in
  which it used to subscribe, and resubscribes after 3 seconds if a stream ends.
- Killed, orchestrator mode and session details are separate reads. A killed read must succeed on a dying root while
  the orchestrator read refuses it, so one combined read would change either delivery or a reply.
- `make generate.check` runs `buf breaking` (`FILE` rules) against the merge base with `origin/main`, read from the
  local `.git`, so it needs no network.

## Consequences

- There is one path into the core for chats: the controller interfaces have one consumer, `internal/localapi`. A
  second chat surface can be written against `api/coagent/v1` alone, and the API socket serves it the same calls.
- Telegram's scenario and unit tests now exercise the real handlers and mappers through the in-process transport.
- Every v1 field is permanent. The breaking gate stops a changed or removed field, and the controller's six ignored
  `chatID` parameters stay until a follow-up removes them.
- Accepted: `/spawn` with a relative or `~` path gets `work_dir must be an absolute path`.
- Accepted: a voice note transcribed to blank gets `text must not be blank`.
- Accepted: new Telegram roots no longer store the unread `channel=telegram` attribute; existing rows keep it.
- Accepted: each Telegram input carries a fresh random idempotency key, so it is never deduplicated, as before.
- Accepted: a send in flight during a config-apply restart completes instead of being cancelled.
- Accepted: an external watcher that stops reading makes the bus drop that author's events with a warning.
- Accepted: listing session details reads every owned root, killed ones included.
- Accepted: with a client's token file a sandboxed session can also use the new chat, settings and discovery calls
  on that client's roots, `SetOrchestratorMode` and `CreateProject` included, inside the already accepted `/config` route.
- Accepted: `ListProjects` shows an API client every daemon project's last activity, whoever owns its sessions, as
  Telegram's `/new` picker already sees it.
- Accepted: the resubscribe path does not run in production in process.
- Accepted: a Telegram call cut off by Telegram's own stop logs `the local API is closing`, and a stream error
  without a message logs as its code name.
- Accepted: Telegram renders an unspecified output type as a plain row; the store holds only the four known types.
- ADR-0104's accepted absence of a `buf breaking` gate no longer holds.

## Alternatives Considered

- **An adapter implementing the controller interfaces over v1.** Telegram would not change, but a permanent
  500-line layer would turn the controller into protobuf on one side of an in-memory wire only for the server to
  turn it back.
- **Keep Telegram on the private controller.** The public contract would stay unproven and the core would keep two
  front doors.
- **Telegram over the API socket with a token.** It adds a socket hop and a credential to the process that already
  owns both ends.
- **Mirror every Go parameter in the schema.** Fields the server ignores would become permanent v1 surface.
- **One `GetSession` read for killed, orchestrator mode and details.** The killed read must succeed on a dying root
  and the orchestrator read must refuse it; backing details by a full listing on every delivery pass is too costly.
- **A committed baseline image for `buf breaking`.** It is an opaque binary that needs a manual re-record, where the
  merge base needs nothing.
