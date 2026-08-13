# Control API

The daemon's control plane (ADR-0007): newline-delimited JSON-RPC 2.0 over a
unix socket.

- **Path:** `~/.coagent/daemon.sock`
- **Mode:** `0600`, owned by the daemon's user; unlinked on shutdown
- **Framing:** one JSON object per line, both directions
- **Liveness:** connecting successfully *is* the check — there is no ping op
- **Readiness:** a bound daemon answers from the bind; until its control plane is
  assembled every op replies `-32000` ("starting")

The constitution's "no inbound listener" rule is about **network** listeners. A
same-user unix socket widens nothing: a process that can open it can already do
anything the daemon can.

## Connecting

The server writes an unsolicited greeting line before reading anything:

```json
{"app":"coagent","binary_version":"0.4.2","protocol_version":1}
```

A client that reads a different `app` dialled the wrong socket. A greeting that
never arrives means the socket is bound by something that is not answering yet —
an older daemon binds long before it serves — which a client treats as
**starting**, never as "not running".
 A different
`protocol_version` is **skew**: surface it as a warning, never refuse. A CLI that
can still read `status` against a slightly older daemon is more useful than one
that will not connect.

### Single instance

The daemon holds an exclusive `flock` on `~/.coagent/daemon.lock` for its whole
life. Only the lock holder may remove a stale socket and bind. A second daemon
exits immediately — two processes on one SQLite file under WAL corrupt each
other, and two socket owners make "which daemon answered?" unanswerable. The
lock is advisory and released by the kernel on death, so a crash never leaves one
nobody can clear. The lock fd is close-on-exec, so the restart-apply exec drops
it and the new image re-acquires it.

## Two directions on one connection

The socket carries both request/response traffic and server→client pushes. A push
is a JSON-RPC **notification**: a `method` and `params`, no `id`.

```json
{"jsonrpc":"2.0","method":"chat_event","params":{"session_id":42,"text":"…"}}
```

A client must therefore be a **demultiplexer**: one read loop, responses matched
to pending calls by `id`, everything else fanned out as a push. Reading "the next
line" as the answer to the last request is wrong — a push can land between a
request and its response, and during a chat it usually does.

## Errors vs. rejections

These are different things and the distinction is load-bearing.

- A **JSON-RPC error** means the transport or the request was broken: malformed
  JSON, unknown method, unparseable params.
- A **rejection** — a guard refusing a mutation, a config that will not load — is
  a *successful* response whose result carries `{"applied": false, "errors": [...]}`.

A client must be able to tell "your input was wrong" from "the daemon is gone".
Codes `-32700`/`-32600`/`-32601`/`-32602`/`-32603` follow the JSON-RPC spec.

`-32000` is the one implementation-defined code: **the daemon is starting**. It
answers *every* op, `status` included, between the bind and the moment the daemon
declares itself ready — the window in which the local chat and the configured
managers are still coming up (a Telegram manager spends several blocking HTTP
round trips there). One answer for the whole window means a half-built op
registry can never look like `unknown method`, and `status` never reports
managers that have not been started yet as down. A client waits and retries; the
same connection carries on into the ready phase (ADR-0017).

## Operations

Op names are `snake_case`.

### `status`

No params. Daemon state, and nothing that has to be probed — provider validity is
proven by use (the chat itself), not by a check here.

```json
{
  "binary_version": "0.4.2",
  "protocol_version": 1,
  "boot_id": "9f3c1a7b0e5d2846",
  "pid": 41337,
  "uptime_seconds": 8040,
  "config_path": "~/.coagent/config.yaml",
  "config_present": true,
  "providers": [
    {"name": "work", "driver": "anthropic"}
  ],
  "model_count": 3,
  "default_model": "claude-sonnet-5",
  "managers": [
    {"id": "telegram-main", "driver": "telegram", "enabled": true, "running": true}
  ]
}
```

`boot_id` names this *run* of the daemon, not the binary. A config apply execs the
same binary into the same pid on the same socket, so a client that must know the
daemon came back — the onboarding bootstrap, a supervisor — compares boot ids for
inequality; version, pid and uptime cannot tell the new run from the old one still
draining. It is opaque and stable for the life of the process.

`config_present: false` is a legal state, not an error: it is what a daemon
reports before onboarding has written anything. Providers, models and managers
are omitted in that state.

A manager that is `enabled` but not `running` carries `error` with the reason,
credential-redacted. A manager failing to start does not take the daemon down —
the chat is how it gets fixed, and it needs the daemon alive to happen.

`coagent status` renders this and pins its exit codes: **0** running, **2** not
running or not ready yet, **1** could not ask. A booting daemon prints
`daemon starting — not answering yet` and exits **2**: like a missing one, it is
a state to retry, not a failure to diagnose.

`running` means the manager's own loops are up, not that it started once.

### `set_provider`

The bootstrap's one write. Adds a provider and, unless `models` says otherwise,
the vendor's recommended model — a provider with no model is a config that
cannot start a session, and the chat that would fix that is the very thing
needing one.

```json
{"name": "work", "driver": "anthropic", "api_key": "sk-ant-…"}
```

`api_key` is the **only** place a credential value crosses this socket. It
travels once: the daemon writes it into `~/.coagent/secrets` under a derived
variable name and puts only `${WORK_API_KEY}` into `config.yaml`. No op ever
reads one back.

Answers with a verdict. On `applied: true` the daemon restarts itself — the
response is written first, then the drain begins, so the caller always has its
answer before the socket goes away. Reconnect by polling until `boot_id` differs
from the one read before the call; 60 seconds is the budget past which the restart
did not work. `applied: true` is not the last word: a config the daemon cannot
boot on is rolled back, and with no session to receive that verdict the only place
it surfaces is the reconnected daemon's own state — so check that what you wrote
is in the status you get back.

A bare `openai` provider has no vendor to key on, so nothing is recommended for
it: pass `models` explicitly.

### `restart_daemon`

No params. Makes the daemon drain and `exec` itself.

```json
{"restarting": true}
```

This is the update path's second half. Onboarding replaces the binary in
`~/.local/bin/coagent` as the plain user, then calls this: the exec path was
captured at boot and now holds the new image, so no service manager and no sudo
are involved. Nothing about the config changes — there is no verdict here, and
no pending-apply marker.

The answer is written before the drain begins, so the caller always has it.
Reconnect by polling and read the greeting's `binary_version` to confirm which
image came back. A daemon too old to know this op answers `-32601`; the caller's
fallback is a full `sudo coagent daemon install`.

### `set_secret`

Stores one credential.

```json
{"name": "MANAGER_TG_BOT_TOKEN", "value": "1234:AAH…", "request_id": "…"}
```

The line is edited in place, so comments and unrelated entries in the secrets
file survive. The value is registered for log redaction immediately.

`request_id` correlates it with a `secret_request` push: supplying one wakes the
session that asked, with the variable *name* and nothing else. Rewriting a
variable that `config.yaml` already references is a rotation, and the daemon
restarts to pick it up.

### `chat_open` / `chat_send` / `chat_stop` / `chat_secret_cancel`

The local chat, served by the built-in CLI manager.

- `chat_open` takes no params, attaches this connection to the chat stream, and
  answers `{"session_id": …, "work_dir": …}`. A zero session id means the
  conversation has not started: the first message creates it. Any masked prompt
  the session is still waiting on is pushed again to the attaching connection —
  `secret_request` is delivered once, so a terminal that missed it would
  otherwise see its messages queue behind a call with nothing on screen.
- `chat_send` takes `{"session_id": …, "text": "…"}`. The message is durably
  appended to the session inbox before acknowledgement. The session consumes it
  at its next causal loop boundary; if idle it is admitted (or queued for
  admission). Answers with the session id, so a client that opened on nothing
  learns what it now belongs to.
- `chat_stop` takes `{"session_id": …}` and interrupts the turn in flight.
- `chat_secret_cancel` takes `{"session_id": …, "request_id": "…"}` and declines
  a masked prompt: nothing is stored, and the call is answered with the fact that
  the user refused, so the session unblocks. It is a chat op rather than a
  valueless `set_secret` because no credential is involved. Only the first
  answer — value or refusal — wins; a later one is refused with the request id
  unknown.

Concurrent terminals are allowed. They all see the same conversation and their
messages interleave — documented behaviour, not a bug.

## Pushes

### `chat_event`

```json
{"session_id": 42, "type": "message", "message": "…", "status": ""}
```

`type` is the session event kind; `status` carries the state on
`state_changed`.

### `secret_request`

```json
{"session_id": 42, "request_id": "…", "name": "MANAGER_TG_BOT_TOKEN", "purpose": "the bot token from BotFather"}
```

The terminal opens a masked prompt and returns the value through `set_secret`
with the same `request_id`, or declines it through `chat_secret_cancel`. It is a
separate method from `chat_event` precisely because a credential must never
travel as chat text. The push is repeated to any terminal that attaches while the
request is still open.

### `secret_resolved`

```json
{"session_id": 42, "request_id": "…", "name": "MANAGER_TG_BOT_TOKEN"}
```

The request is over — answered or declined, by this terminal or another one — so
a client still showing that prompt closes it and discards whatever was typed
there. It is pushed once per request, to every attached terminal including the
one that resolved it, which recognises its own answer coming back. Without it the
terminals that lost the race sit at a masked prompt that swallows the user's next
message into a `set_secret` the daemon refuses.
