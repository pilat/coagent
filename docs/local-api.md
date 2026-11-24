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
from manager ids because operator clients key chat rows; hashes must be unique. Roles are `operator` and `app`.
Every request on the socket,
RPC or not, is refused without a valid token in `Authorization: Bearer <token>`.
Config keeps only the token's SHA-256.
Removing the entry and restarting revokes it. Every operator client reaches every root, as every manager does;
each keeps its own chat focus and cursor. An app holds delegated authority and keeps no chat.

`operator` names the person who owns the machine, config and sandbox rules. Its input is human input,
including session commands and `/config`; the host stamps its source, never the request.
A read-only host mount still permits connecting to a Unix socket, so socket mode alone is no boundary.
An operator client's plaintext token file is as readable from the sandbox as any operator credential; with it a
session can send `/config` to every root and edit config, or use the chat, session-setting and
discovery calls on every root. With the sandbox off, a session already has the user's authority.
See [ADR-0104](adr/0104-the-local-api-is-the-two-durable-ledgers.md) and
[ADR-0105](adr/0105-built-in-managers-are-local-api-clients-in-process.md).

To delegate to a program, change the printed `role: operator` to `role: app`. Add `projects` when the delegation
should reach only named work directories; omit the key to reach every project. An explicit empty list is refused:

```yaml
api:
  clients:
    - id: build-program
      role: app
      token_sha256: <64 lowercase hex characters>
      projects:
        - /absolute/project/path
        - /another/project
```

Each project must be an absolute, cleaned path with no duplicates; the paths need not exist at config load.
`projects` is refused on an operator. Project scope compares registered work directories, never project names or
session ownership. It covers a root in a listed directory and a coagent-created worktree whose `repo_root` is
listed and whose `coagent_worktree_origin` is `controller-created-v1`. Reads and writes both use that rule;
lists omit roots and projects outside the project scope. Such a root returns `session N not found`, identical
to a missing id; unknown and out-of-project-scope create paths both return `no project at "<dir>"`.

A project that is a subdirectory of its repository does not bring along that repository's worktrees: an app may
create one there and then receive `session N not found` for it. For symlinked paths, list the work directory as
the daemon registered it (`filepath.Abs`) and the repository path as git reports `repo_root`.
Project scope limits access through coagent; sessions still use the host-readable sandbox default, including
other clients' plaintext token files. This is a single-tenant delegation, with no owner or tenant visibility rule.

## Calls

- `SessionService.CreateSession` takes an absolute `work_dir`, optional configured `model` id and optional
  `worktree_name`. A worktree name forks the directory's repository into that named worktree and runs the root
  there; empty runs in `work_dir`. It creates an empty root; send the first input separately. Retrying a lost
  create response can leave an extra empty root. An app may create only in an existing, visible registered
  project within its project scope that is not itself a coagent worktree. Its model is the default or a tagged
  candidate, at that model's default effort; the orchestrator's capacity headroom check applies.
- `SessionService.SendInput` takes `session_id`, non-blank `text` and a client-chosen `idempotency_key` of
  1–128 bytes. Reuse a key only to retry the same input. Keys are per root and author: two clients may use the same
  key without colliding. An operator sends human input; an app sends a sent message, with session commands kept
  as plain text. The model sees `[Message from the program "<id>" — sent through the local API, not by the human]`
  before the text; the chat shows `↪ from app <id>`. An inserted sent message from the creating app names an
  untitled root when its text yields a title. Text beginning with `/` leaves it untitled, so a later message can
  name it; a `/clear` successor is eligible again. Existing titles, including human-set titles, are preserved.
  An accepted send runs to completion even if the client disconnects or its deadline passes. Uniqueness is
  per root, through the inbox delivery key `user:<author>:<key>` or `app:<author>:<key>`; a retry never appends twice.
  Rows from before the upgrade retain `user:<key>`, so a retry crossing that upgrade can append once more. A repeated key
  answers OK once the input is on the root. It does not report whether a command finished; the tail does.
  A replaced root resolves to its successor. For operator command input, a retried `/clear` that already replaced
  the root clears its
  empty successor too, and a retried `/kill`, or `/stop` while the root is still stopping, is refused because
  the root no longer accepts input. If the first send persisted but could not start the runner, a repeated
  key succeeds without starting it; fresh input or restart resumes it.
  Built-in managers use the same author prefix with fresh random keys. Their configured ids are not format-validated;
  a colon in a manager id can make its author/key prefix ambiguous with another manager's. Use short word ids.
- `SessionService.StopSession` and `KillSession` take a positive `session_id` and return the root the command
  reached, following `/clear` replacements. For an operator they enqueue the human `/stop` or `/kill`. An app
  may end only roots it started; another root returns `session #N was not started by this app; you can stop or
  kill only sessions you started`. The host writes the fixed command with app provenance; acknowledgements end
  `— asked by app <id>`, and in-daemon watchers are told `was stopped by app <id> with StopSession` or
  `was killed by app <id> with KillSession`. Accepted commands outlive a disconnected client. The response
  distinguishes neither "nothing to stop" nor "already killed"; read the tail and turn outcome for the effect.
- `SessionService.GetTurnOutcome` takes a positive `session_id` and returns the newest root's `session_id`
  plus `over`. It reads durable pending input, background wake sources and runner state. A new empty root,
  parked root, or killed/terminating root is over; a running, waiting or stopping turn is not. Every root waiting
  for admission is not over, even with no pending inbox row. For an errored root, fresh app or scheduled input
  received after the recorded error keeps `over` false, even before the wake goroutine processes it.
  Input received during the failed run does not restart it and remains
  compatible with `over=true` once no runner or admission wait remains.
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
  Wake hints are unfiltered even for a project-scoped app: every root's activity id is visible, without content.
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

## App refusals

An app can use session creation, sends, lists, reads and tails, turn outcomes, discovery reads and its own roots'
stop/kill calls. `ListModels` lists every configured model; an explicit create choice must still be tagged,
and a model refusal lists the tagged candidates. Shared daemon refusals keep their orchestrator wording, such as
`only the human can resume it` for a budget park and `kill_session` for the corresponding `KillSession` call.
A sleep interrupted by an app message retains the existing notice, "another session sent a message".

The remaining calls are refused before any side effect:

| Call | Refusal |
|---|---|
| `SetSessionModel` | `an app cannot change a session's model` |
| `SetSessionTitle` | `an app cannot rename a session` |
| `SetOrchestratorMode` | `an app cannot change orchestrator mode` |
| `CreateProject` | `an app cannot create projects` |
| Every `ChatService` call | `an app keeps no chat; follow a root with TailOutput` |

An app has no `/config`, `/skill`, `/orchestrate` or other human-input authority in its text. The fixed host-written
stop and kill commands are its only delegated session commands. A project-scoped app's `SendInput` is refused
when the newest root in the replacement chain has orchestrator mode on:
`orchestrator mode is on for session #N; a project-scoped app cannot send to it`. An unscoped app may send there;
reads remain allowed. The check and enqueue are separate: enabling orchestrator mode in the instant between them
can let one message through.

## Waiting for a turn

Subscribe to `StreamSessionEvents` and wait for its initial `ACTIVITY` event with id 0. Send with `SendInput`,
then call `GetTurnOutcome` immediately. After each wake hint, read or tail new output and call `GetTurnOutcome`
again until `over` is true. Also re-read periodically because hints can be dropped. Keep the returned newest
`session_id` when a `/clear` replacement is reported. Output rows and best-effort runtime state alone do not
prove the turn is over.

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

curl --unix-socket ~/.coagent/api.sock http://localhost/coagent.v1.SessionService/GetTurnOutcome \
  -H "Authorization: Bearer $COAGENT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"session_id":"1"}'
```
