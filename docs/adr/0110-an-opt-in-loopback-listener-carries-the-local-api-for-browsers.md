# ADR-0110: An opt-in loopback listener carries the local API for browsers

- **Status:** Accepted
- **Date:** 2026-11-24
- **Amends:** [ADR-0104](0104-the-local-api-is-the-two-durable-ledgers.md)

## Context

A browser cannot open a Unix socket. The operator wants a browser on the same machine to reach `coagent.v1`,
but CLAUDE.md and ADR-0104 describe a daemon with no network listener. The local API already authenticates
every API-socket request before Connect, binds its author to the appropriate controller capability, and
strips the bearer before the handler sees it. A second transport needs the same guarantees.

Sessions share the host network namespace ([ADR-0063](0063-explicit-filesystem-boundary.md)). A sandboxed
session already reaches `api.sock` through the read-only host bind; its token, rather than socket mode,
prevents it from acting as an API client.

## Decision

Add one optional loopback listener to `localapi.Server`. `api.listen` is empty by default: no network bind,
goroutine or log line is added while disabled. When enabled, it carries the same authenticated handler value
as `api.sock`: one Connect server, registration, clients and token check, wrapped by one browser guard.
There is no second mux or authority model, Web UI, static file serving, new RPC, configuration mutation or
secret-bearing API. `DaemonService.GetStatus` remains exclusive to the anonymous `daemon.sock`; on TCP it
returns 404 with a token and 401 without one, like an unknown procedure.

Configuration uses the existing flat API section:

```yaml
api:
  listen: 127.0.0.1:7878
  origins:
    - http://localhost:5173
  clients: [...]
```

`listen` requires at least one API client, a canonical loopback IP literal and an explicit nonzero port.
Names, wildcard or LAN addresses, IPv6 zones and IPv4-mapped IPv6 addresses are refused. The bind helper
independently parses the address and refuses non-loopback and IPv4-mapped literals before listening.
There is no key to permit remote binding. Remote access is the operator's tunnel or proxy; the daemon
implements no TLS. Loopback binds only after the API socket attaches successfully. Bind failure logs
`local_api_start_failed` with the address in `socket`, leaving the daemon and API socket serving.
Serve failures use `local_api_serve_failed`; neither requests nor Host, Origin or token values are logged.

`origins` requires `listen`. Entries are distinct, lowercase HTTP(S) origins with a hostname, no credentials,
path, query or fragment, and no opaque URL or query delimiter. Explicit ports must be canonical decimal
1–65535 and omit the scheme default. `*` and `null` are refused.

The browser guard enforces this order:

1. `r.Host` equals the bound listener address or `localhost:<same port>`, case-insensitively. These two
   allowed hosts come from the bound listener. Anything else, including empty, returns 403 before origin
   or token work. DNS rebinding retains the attacker's hostname in Host, so it cannot pass this rule.
2. An absent Origin passes. A present Origin must match `api.origins` exactly; `null` is always refused.
   Host and origin refusals use the same plain `forbidden` body without CORS headers or Connect work.
3. `OPTIONS`, a path starting with `/`, an allowed Origin and a requested-method header form a preflight.
   The guard answers 204 with an empty body, without authentication, equally for known and unknown paths.
4. All other requests with an allowed Origin receive actual-response CORS headers before the unchanged
   token check and Connect handler. A browser can therefore read an authentication refusal.

The static preflight response echoes the allowed origin and sets:

```text
Access-Control-Allow-Methods: POST
Access-Control-Allow-Headers: Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-Grpc-Web, X-User-Agent
Access-Control-Max-Age: 7200
Vary: Origin, Access-Control-Request-Method, Access-Control-Request-Headers
```

Both response types echo the allowed origin in `Access-Control-Allow-Origin`. Actual responses expose
`Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin`, and set `Vary: Origin`. Requests without Origin carry
no CORS headers and need the token as on `api.sock`. All RPCs are POST; a browser supplies Origin on them.
`OPTIONS *` and OPTIONS without a requested method fall through to authentication. There are no cookies
or `Access-Control-Allow-Credentials` responses. The header lists follow the Connect CORS documentation
and `connectrpc.com/cors`, with Authorization explicitly added.

Stopping cancels the shared base context first. Status connections close immediately; API socket and
loopback calls drain under one two-second grace deadline, then force-close, including stalled streams.
Explicit listener closes release binds even if serving has not begun.

## Consequences

- On TCP the 256-bit token is the only boundary. Every local OS user and every container in the host
  network namespace can connect; there is no peer-UID check.
- A sandboxed session reaches the loopback listener like any loopback service and gains nothing beyond
  `api.sock`: with a readable token file it could already act through that socket (ADR-0104).
- A tunnel changing the port is refused by Host. Keep the port (`-L 7878:127.0.0.1:7878`) or set Host at the proxy.
- The loopback listener needs `api.sock`. An occupied socket path prevents TCP startup too; API startup
  failure is logged. This dependency is accepted for a broken home directory.
- There is no `Access-Control-Allow-Private-Network` answer. Public pages may meet the browser's own
  local-network permission prompt; localhost pages do not.
- There is no same-origin page. The daemon serves no files; every browser caller must be listed in `api.origins`.
  A future Web UI stage decides whether the listener's own origin is implicitly allowed.
- Streaming is proven with a Go client over HTTP/1.1 and browser-like headers. A real browser's fetch streaming
  and CORS enforcement are not exercised in CI.
- The preflight answer is static, without reflecting or checking requested headers or method. The browser
  enforces the answer; an unlisted custom header fails there.
- Tests probe a free port then bind it; a port taken in between fails a test run, not the product.
- There is no rate limit or lockout on bad tokens. Guessing a 256-bit token is not a practical route.
- There is no connection limit or idle timeout. Any local OS user can hold connections open, as the same user
  already could on `api.sock`; a local denial of service is not defended against.
- On stop the servers drain one after the other, so while an `api.sock` call drains, the loopback listener
  accepts for up to the shared grace; such calls start on a cancelled context.

## Alternatives Considered

- **Wildcard or LAN binding behind a second key.** Remote access belongs to the operator's tunnel or proxy.
- **An `api.hosts` list.** It widens the DNS-rebinding guard for a rare tunnel-port case.
- **Peer-UID checks.** Linux-only and racy, and stricter than the chosen token model.
- **`connectrpc.com/cors` plus `rs/cors`.** Two dependencies for three lists and a small guard.
- **TLS in the daemon.** Certificate termination is a deployment decision.
- **Anonymous GetStatus on TCP.** Every TCP call must carry a token; status stays on the control socket.
- **A second mux with its own authentication.** It creates two paths whose contracts must remain in step.
