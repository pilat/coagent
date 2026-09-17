# Control API

The daemon's control plane: a read-only status protocol over a same-user unix
socket — newline-delimited JSON-RPC 2.0, one direction. ADR-0060 removed the
chat transport, configuration mutations, secret protocol, restart operation and
server pushes this socket once carried; what remains is greeting/readiness and
one `status` method.

- **Path:** `~/.coagent/daemon.sock`
- **Mode:** `0600`, owned by the daemon's user; unlinked on shutdown
- **Framing:** one JSON object per line
- **Direction:** requests in, responses out. There are no server pushes; the
  next line after a request is always its answer
- **Liveness:** connecting successfully *is* the check — there is no ping op

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
**starting**, never as "not running". A different `protocol_version` is
**skew**: surface it as a warning, never refuse. A CLI that can still read
`status` against a slightly older daemon is more useful than one that will not
connect.

### Single instance

The daemon holds an exclusive `flock` on `~/.coagent/daemon.lock` for its whole
life. Only the lock holder may remove a stale socket and bind. A second daemon
exits immediately — two processes on one SQLite file under WAL corrupt each
other, and two socket owners make "which daemon answered?" unanswerable. The
lock is advisory and released by the kernel on death, so a crash never leaves one
nobody can clear.

## Errors

Malformed JSON, an unknown method, unparseable params and the rest follow the
JSON-RPC spec codes `-32700`/`-32600`/`-32601`/`-32602`/`-32603`.

`-32000` is the one implementation-defined code: **the daemon is starting**. It
answers *every* op, `status` included, between the bind and the moment the daemon
declares itself ready — the window in which the configured managers are still
coming up (a Telegram manager spends several blocking HTTP round trips there).
One answer for the whole window means a half-built op registry can never look
like `unknown method`, and `status` never reports managers that have not been
started yet as down. A client waits and retries; the same connection carries on
into the ready phase (ADR-0017).

## Operations

### `status`

The only method. No params, read-only by design: every configuration change
travels through a manager session's `/config` flow, not through this socket.

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
  "default_model": "",
  "search": "tavily",
  "managers": [
    {"id": "telegram-main", "driver": "telegram", "enabled": true, "running": true}
  ]
}
```

`boot_id` names this *run* of the daemon, not the binary. A config apply
restarts the daemon and it comes back on the same socket, so a client that must
know the daemon came back compares boot ids for inequality; version, pid and
uptime cannot tell the new run from the old one still draining. It is opaque and
stable for the life of the process.

`config_present: false` is a legal state, not an error: it is what a daemon
reports before the operator has written a valid `config.yaml`. Providers, models
and managers are omitted in that state.

`search` renders the integrated-search state: `tavily`, `searxng (<base_url>)`,
`native (openrouter)`, `disabled`, or absent when unconfigured.

A manager that is `enabled` but not `running` carries `error` with the reason,
credential-redacted. A manager failing to start does not take the daemon down —
its Telegram service topic is how it gets fixed, and it needs the daemon alive
to happen. `running` means the manager's own loops are up, not that it started
once. A manager entry may also carry `pending_outputs`, `blocked_output_id`,
`blocked_for_seconds` and `delivery_error` describing its output-delivery
backlog; a manager ID no longer in the configuration can still appear with
driver `removed` until its undelivered backlog drains.

`coagent status` renders this and pins its exit codes: **0** running, **2** not
running or not ready yet, **1** could not ask. A booting daemon prints
`daemon starting — not answering yet` and exits **2**: like a missing one, it is
a state to retry, not a failure to diagnose.

## Removed operations

The socket once carried `set_provider`, `set_secret`, `restart_daemon`, the
`chat_*` family, and unsolicited `chat_event`/`secret_request`/`secret_resolved`
pushes. All are gone. Any other method name answers:

```json
{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"unknown method set_provider"}}
```

There are no compatibility no-ops: no supported client exists, and a silent
no-op would conceal a stale caller instead of failing it (ADR-0060). There is
nothing to fall back to — the operator edits `~/.coagent/config.yaml` and
`~/.coagent/secrets` by hand and uses explicit `coagent daemon ...` commands.
