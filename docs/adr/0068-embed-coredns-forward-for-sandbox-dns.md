# ADR-0068: Embed CoreDNS forwarding for sandbox DNS

- **Status:** Accepted
- **Date:** 2026-09-25

## Context

The custom DNS relay served one request per UDP endpoint. System resolvers send
A and AAAA queries together on the same socket, so the second query was
discarded and clients waited for a retry. Implementing DNS framing, connection
reuse, failover and health checks duplicates a mature protocol component.

The sandbox cannot simply be pointed at the host's resolver: that resolver is
often a loopback stub or a VPN-provided address that only resolves correctly
from the host's own network position. Something has to relay.

## Decision

Embed the CoreDNS `forward` plugin and its standard DNS serving library. Each
network generation runs one service and binds UDP and TCP listeners on the host
side of its uplink. The generation's ruleset redirects port 53 at the sandbox's
default gateway address to those listeners, for IPv4 and IPv6 alike, so the
resolver is reachable at exactly one address and nowhere else.

Upstreams are literal addresses from the host resolver configuration, captured
when a generation starts. Their outbound sockets use the host network, so a
loopback stub or VPN resolver remains reachable. Sandbox DNS requests cannot
select arbitrary upstream addresses. Missing resolver configuration fails
generation creation explicitly.

CoreDNS owns DNS exchanges, upstream selection and health checks. Our adapter
limits concurrent requests and TCP connections and couples shutdown to the
existing generation lifecycle. It explicitly owns CoreDNS transport cleanup
instead of waiting for finalizers. The listeners support repeated and concurrent
queries without closing the client endpoint after one reply.

## Consequences

- The same DNS path works for shell processes, MCP and built-in web tools.
- The binary gains CoreDNS and DNS-library dependencies but needs no installed
  DNS helper, public listener or separate daemon.
- The resolver's listeners live in the host namespace. Only the generation's
  redirect makes them reachable, and only from its own guest interface; nothing
  else on the host is pointed at them.
- Shutdown drains in-flight requests before the generation's links are removed.
  CoreDNS may finish an in-flight upstream dial on its own timeout after that;
  its cache and health-check workers stop asynchronously and accept no further
  workload requests.
- A generation captures the host's resolvers once. A host resolver change does
  not reach a running generation; the next one picks it up.

## Alternatives Considered

- **Maintain the custom relay.** Rejected because protocol correctness and
  resolver compatibility are better owned by an established implementation.
- **Give the sandbox the host's resolver addresses directly.** Rejected because
  a loopback stub or VPN resolver is not reachable or not correct from the
  sandbox's network position, and it would need a grant per resolver address.
- **Use a public DNS server directly.** Rejected because host-local and VPN
  resolver behavior must remain available.
