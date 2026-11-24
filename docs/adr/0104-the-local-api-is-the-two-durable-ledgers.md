# ADR-0104: The local API is the two durable ledgers, behind a bearer token

- **Status:** Accepted
- **Date:** 2026-11-23
- **Amended by:** [ADR-0105](0105-built-in-managers-are-local-api-clients-in-process.md)
- **Amended by:** [ADR-0106](0106-one-operator-every-surface-reaches-every-root.md) — every client reaches every root
- **Amended by:** [ADR-0107](0107-every-root-is-a-root.md)

## Context

Programs outside the daemon need to start work, send input and follow what a session produces. Until now the only
external surface was the status-only control socket; the controller contract is private to built-in managers.

The daemon once carried a chat over that socket. [ADR-0060](0060-telegram-service-topics-own-daemon-management.md)
deleted it: it was RPC plus best-effort pushes with no replay ([ADR-0021](0021-best-effort-control-pushes.md)), so
a disconnect lost output, and keeping it correct took suspension, pushes and terminal interception across several
packages. Since then both directions became durable ledgers: every input enters `session_inbox` before anyone acts
on it, and every user-visible output of a manager-owned root is an append-only `session_outbox` row with one
table-wide AUTOINCREMENT id ([ADR-0030](0030-durable-session-outbox.md),
[ADR-0072](0072-flat-chat-with-a-focus.md)). An API over those ledgers needs no push protocol at all.

A sandboxed session sees the host read-only, and a read-only mount does not stop it connecting to a same-user unix
socket, so the socket's mode cannot be what keeps a session from acting as the operator.

## Decision

- `coagent.v1` is a Protocol Buffers schema (`api/coagent/v1/coagent.proto`) served with ConnectRPC (connect-go v2)
  on `net/http` over HTTP/1.1, on a second socket, `~/.coagent/api.sock` (mode 0600). Only the single-instance lock
  holder binds it, and only when `api.clients` is non-empty. `daemon.sock` and `coagent status` are unchanged: their
  greeting is written before anything is read ([ADR-0017](0017-a-bound-control-socket-answers.md)), so HTTP cannot
  share that socket without changing status clients.
- The API is the two ledgers. `SendInput` appends human input to `session_inbox` under a client-chosen idempotency
  key, stored as the existing delivery key `user:<key>`; a repeated key for the same root appends nothing. An
  accepted send runs under the server's lifetime, not its client's connection or deadline, as a manager's does.
  `TailOutput` streams `session_outbox` rows above an id the client holds: catch-up from SQLite, then live, woken by
  the client's session events with a fallback read. It continues through `/clear` replacements and ends cleanly
  only after a killed root's last row. Reconnecting is the same call with the `session_id` and id of the last row
  received. The running/idle runtime state rides along as best effort and is never the source of truth.
  `CreateSession` creates an empty root, so the first input also goes through the keyed send.
- Every call carries `Authorization: Bearer <token>`. `coagent token <id>` mints a 256-bit random token, prints it
  once with the config lines to paste, and edits nothing. `config.yaml` keeps only `token_sha256`; comparison is
  constant-time; removing the entry revokes the token at the next config apply. The server checks the token in
  front of the RPC layer, on every request to the socket, and removes the bearer value from the request before any
  handler runs, so nothing an accepted call hands on can yield it.
- Authority follows the source the host stamps on an input, as it already does inside the core. Every call has
  an author: the operator, an app, a session or a schedule. An author acts either with human authority or with
  authority a human delegated, and a delegation may be limited to projects: `/orchestrate` delegates to a session,
  and an app token is the same delegation given to a program. A transport only authenticates the author. It
  carries no authority of its own, and the API server holds no role table and recognizes no commands. The host
  stamps the input's source and sender attributes from the authenticated author, never from the request.
- The only author served over the API so far is the `operator`: the person who owns the machine, `config.yaml` and
  the sandbox rules. Its input is human input, session commands and `/config` included. "Human input" names the
  input class and "operator" names who holds the token; they are not two roles. A config entry's `role` names the
  author kind, and validation accepts only `operator` until the app author is served.
- Each client is bound to its own owner id through the manager-bound controller factory, the mechanism of
  [ADR-0023](0023-manager-owned-session-publication.md): client ids share the namespace of manager ids, and a
  client owns and sees only the roots it created.
- No RPC mutates configuration, carries secrets or streams model output token by token. A configuration change runs
  only through a session's `/config` flow.
- The v1 surface is designed whole and grows additively: chat focus and cursor as durable server state in a
  `ChatService`, session reads, titles, models and orchestrator mode, discovery, and status. Refusals carry the
  controller's own text so a manager can move onto v1 and show the same lines.

## Consequences

- A client can create work, send input and follow it across disconnects and daemon restarts with no lost and no
  repeated row, because the position is an id in an append-only log it holds itself.
- There is one path: the server uses the manager-bound controller for creation, input and reads. It adds no
  session flow of its own.
- The module gains connect-go v2 and protobuf; generated code is committed, its tools (`buf`, `protoc-gen-go`,
  `protoc-gen-connect-go`) are pinned by `make tools`, and `make generate.check` keeps it current.
- `~/.coagent` holds a second socket.
- Accepted: with the sandbox off a session already has the user's authority.
- Accepted: a client's plaintext token file is as readable from the sandbox as any operator credential, and with it
  a sandboxed session can send `/config` to that client's roots and have `config.yaml` edited.
- Accepted: a retried `CreateSession` after a lost response leaves an extra empty root.
- Accepted: a refusal can confirm to one operator client that a root id exists under another owner.
- Accepted: following a tail needs a Connect, gRPC-Web or `buf curl` client; plain `curl` reaches only the unary
  calls.
- Accepted: a repeated key whose first attempt committed its input but failed to start the runner returns success
  without starting it; the input waits for the next fresh input or a restart.
- Accepted: a repeated `/clear` under one key, after the first replaced the root, clears the empty successor too.
  A repeated `/kill`, or `/stop` while the root is still stopping, is refused because the root no longer accepts
  input. A repeated key reports that the input is on the root, not whether its command finished.
- Accepted: a row a replaced root commits after its replacement is not streamed, as a chat never shows it.
- Accepted: if a replacement and such a late row both commit between two adjacent reads of one tail pass, the tail
  sends the late row and skips the successor's rows below it.
- Accepted: runtime state is best effort and sent only on change; a dropped event delays a live row until the
  fallback read.
- Accepted: a root id 16 or more replacements old stops resolving; clients address the newest `session_id`
  they received.
- Accepted: a tail on a quiet root sends no response headers until its first row or state change.
- Accepted: a first tail read that fails for a transient store reason reads as a refusal.
- Accepted: a stale `api.sock` left by a crash stays on disk after the clients are removed; connecting is refused.
- Accepted: there is no `buf breaking` gate until a published v1 message first changes.
- An API client's roots and Telegram's are invisible to each other until one operator can own roots from several
  surfaces.

## Alternatives Considered

- **Extend the JSON-RPC control socket.** It would rebuild the chat ADR-0060 deleted, with no schema for clients
  and the push protocol that lost output.
- **Share `daemon.sock` by sniffing the first bytes.** The server writes first, so sniffing changes the greeting
  and readiness contract every status client relies on.
- **gRPC on grpc-go.** It needs HTTP/2, browsers need a proxy, and it brings a second HTTP stack.
- **Hand-written REST plus server-sent events.** No schema or generated clients, and two encodings to keep in step.
- **connect-go v1.** v2 is the current major, and its in-process transport lets a built-in manager use the same
  contract.
- **Checking the token in a connect interceptor.** connect copies the request headers into the call and keeps the
  request, so an interceptor can hide the bearer value from handlers but cannot remove it; net/http middleware in
  front of connect can, and it also covers requests that are not RPCs.
- **A server-side cursor per client.** It needs a new table and its transitions; an id the client holds suffices
  because output ids are table-wide and only grow.
- **One stream over all of a client's roots.** It needs a new store read; a per-root tail reuses the existing one
  and can be added later.
- **Socket mode alone.** A sandboxed session can connect through its read-only host view.
- **Plaintext tokens in config or the secrets file.** `/config` reads the whole document; a hash of a 256-bit random
  token is enough to verify it and needs no salt or slow KDF.
- **Refuse `/config` over the API.** It would make the operator role less than the person it names; the owner kept
  full human authority and accepted the token-file route instead.
