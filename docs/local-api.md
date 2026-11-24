# Local API

## Socket and protocol

`coagent.v1` is the public Protocol Buffers contract in
[`api/coagent/v1/coagent.proto`](../api/coagent/v1/coagent.proto). ConnectRPC serves it on
`~/.coagent/api.sock`, a same-user Unix socket with mode `0600`, bound only when `api.clients` is non-empty.
It uses HTTP/1.1 and accepts Connect JSON or binary messages and gRPC-Web. There is no TCP listener.
Unary JSON calls work with `curl`; streams use envelopes and need a Connect, gRPC-Web or `buf curl` client.
The separate `daemon.sock` still serves only [`coagent status`](control-api.md).
Built-in managers use the same services over an in-process connection, with the configured manager id as
author and no socket or token.

## Clients and tokens

Run `coagent token <id>` without a daemon or config. The id must match `^[a-z0-9][a-z0-9_-]{0,31}$`.
The command prints the plaintext token once, followed by this config shape with its actual hash:

```yaml
api:
  clients:
    - id: my-client
      role: operator
      token_sha256: <64 lowercase hex characters>
```

Paste the printed block into `~/.coagent/config.yaml` and restart. Client ids must be unique and distinct
from manager ids because they key chat rows; hashes must be unique. Only `operator` is accepted.
Every request on the socket,
RPC or not, is refused without a valid token in `Authorization: Bearer <token>`.
Config keeps only the token's SHA-256.
Removing the entry and restarting revokes it. Every client reaches every root, as every manager does;
each keeps its own chat focus and cursor.

`operator` names the person who owns the machine, config and sandbox rules. Its input is human input,
including session commands and `/config`; the host stamps its source, never the request.
A read-only host mount still permits connecting to a Unix socket, so socket mode alone is no boundary.
A client's plaintext token file is as readable from the sandbox as any operator credential; with it a
session can send `/config` to every root and edit config, or use the chat, session-setting and
discovery calls on every root. With the sandbox off, a session already has the user's authority.
See [ADR-0104](adr/0104-the-local-api-is-the-two-durable-ledgers.md) and
[ADR-0105](adr/0105-built-in-managers-are-local-api-clients-in-process.md).

## Calls

- `SessionService.CreateSession` takes an absolute `work_dir`, optional configured `model` id and optional
  `worktree_name`. A worktree name forks the directory's repository into that named worktree and runs the root
  there; empty runs in `work_dir`. It creates an empty root; send the first input separately. Retrying a lost
  create response can leave an extra empty root.
- `SessionService.SendInput` takes `session_id`, non-blank `text` and a client-chosen `idempotency_key` of
  1–128 bytes. Reuse a key only to retry the same input. Keys are scoped to the root, which every client
  reaches: choose keys unique across clients, for example a random 128-bit value.
  An accepted send runs to completion even if the client disconnects or its deadline passes. Uniqueness is
  per root, through the existing inbox delivery key `user:<key>`; a retry never appends twice. A repeated key
  answers OK once the input is on the root. It does not report whether a command finished; the tail does.
  A replaced root resolves to its successor. A retried `/clear` that already replaced the root clears its
  empty successor too, and a retried `/kill`, or `/stop` while the root is still stopping, is refused because
  the root no longer accepts input. If the first send persisted but could not start the runner, a repeated
  key succeeds without starting it; fresh input or restart resumes it.
- `OutputService.TailOutput` takes `session_id` and `after_id` (0 for the whole log). It catches up, then
  follows live, continuing through `/clear`. It ends cleanly only after a killed root's last row. Each row
  has its own `session_id`. Keep the `session_id` and `id` of the last row received and use that session id
  for both tail and send: an id 16 or more replacements old stops resolving. Rows a replaced root commits
  after replacement are not streamed. Runtime state is best effort, sent only on change; a missed wake delays
  output until a fallback read. The client holds its cursor; reconnecting does not replay rows at or below it.
- `SessionService` also serves session reads and settings. `ListSessions` lists the live roots,
  newest activity first; `ListSessionDetails` lists every root, killed ones included;
  `GetSessionKilled` reports whether a root has been killed. `SetSessionModel` switches a root's
  model and reasoning effort; `SetSessionTitle` renames a live root. `GetOrchestratorMode` reports whether
  a live root has orchestrator mode; `SetOrchestratorMode` turns it on or off.
- `OutputService` also serves bounded reads and wake hints. `ListOutput` returns up to `limit` rows of a root
  above `after_id`, oldest first. `ReadEntry` returns the newest renderable rows and newest output id from one
  read, oldest first, leaving out `session_opened` and `session_replaced` rows. Both require `limit` in 1–100.
  `StreamSessionEvents` streams root ids with activity or heartbeat, never content. Its first event is `ACTIVITY`
  with `session_id` 0: the subscription is live, so state read after it misses no later change. Hints may be
  dropped under load; re-read state on any event and periodically.
- `ChatService` keeps one chat's focus and delivery cursor as durable server state. `BindChat` creates its row,
  preserving an existing focus and cursor. `GetChatFocus` returns the focused root, or 0;
  `GetChatCursor` returns focus, cursor and whether entry has been acknowledged. `EnterChat` acknowledges entry
  at `newest_id` while `session_id` is the focus and entry is still owed. `AdvanceChat` moves the cursor to
  `output_id` while `session_id` is the entered focus and the cursor is below it. `SetChatFocus` focuses a live
  root and resets the cursor, owing entry. `ResetChatFocus` clears focus and cursor while focus is still
  `from_session_id`.
- `DiscoveryService` lists what the caller can start work with. `ListModels` lists configured models;
  `ListSkills` lists skills available to a root; `ListProjects` lists daemon-managed projects, most
  recently active first; `ListSpawnFavorites` lists configured favorite project paths. `CreateProject` creates
  a folder project under the projects root, or opens an existing one.

## Errors and retries

`Unavailable` and transport errors mean reconnect with the last received session id and row id.
`InvalidArgument` (malformed request) and `Unauthenticated` (missing or wrong token) never succeed unchanged.
`FailedPrecondition` carries the controller's refusal text and may succeed after the session's state changes;
the message says why. Retry sends under the same key. A first tail read failure is a refusal; a later read
failure is `Unavailable`.

## Unary JSON examples

Set `COAGENT_TOKEN` to the token printed by `coagent token`. JSON int64 values are returned as strings.

```sh
curl --unix-socket ~/.coagent/api.sock http://localhost/coagent.v1.SessionService/CreateSession \
  -H "Authorization: Bearer $COAGENT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"work_dir":"/absolute/project/path"}'

curl --unix-socket ~/.coagent/api.sock http://localhost/coagent.v1.SessionService/SendInput \
  -H "Authorization: Bearer $COAGENT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"session_id":"1","text":"/status","idempotency_key":"status-1"}'
```
