# ADR-0064: Embedded transparent gateway for sandbox networking

- **Status:** Superseded by [ADR-0069](0069-kernel-routed-sandbox-network.md)
- **Date:** 2026-09-23

## Context

Filesystem confinement alone leaves the host network and abstract Unix sockets
available to workloads. Profiles also need to describe deliberate access to
host or private-network services. A proxy configured only through environment
variables is not an enforcement boundary and does not work transparently for
the breadth of Linux development clients the default profiles must support.

The system should preserve public internet access and local development servers
without intercepting TLS. Networking must share the lifetime of the workloads
whose authority it enforces, including shell activation, background jobs and
MCP clients in an active tool stack. Host-side web tools must not bypass the same boundary.

## Decision

Use a private network namespace and transparent TCP/UDP gateway for each root
session tree and immutable policy generation. Embed the gateway in the coagent
binary using a pinned gVisor userspace TCP/IP library. Do not implement a TCP
stack or require an additional installed network-helper program. A narrowly
scoped same-binary setup/entry mechanism owns namespace and TUN setup; user
code executes only after setup authority and descriptors have been removed.

The gateway handles IPv4/IPv6 TCP and UDP. Public egress is permitted by default;
host interfaces, host loopback, private/LAN and special-use destinations require
profile grants under [ADR-0063](0063-default-project-confinement-with-tool-profiles.md).
Destination decisions use the actual connection IP and protocol/port, not
permission inferred from a DNS answer. Sandbox DNS is supplied through the
gateway. TLS remains end-to-end with no certificate substitution, payload
inspection or domain allowlist claim.

Sandbox loopback is private and shared within a tree, including its shell,
MCP and LSP processes. Unrelated roots use different namespaces. Explicit
host-loopback access uses the reserved `host.coagent.internal` alias and profile
grants; `localhost` never silently means the daemon host. No sandbox service
ports are automatically published on the host.

Built-in web fetch and REST search transfer a connected socket from a helper
running inside the tree's namespace, so DNS and destination authorization pass
through the same gateway as shell processes. In particular, fetching a
development server on localhost reaches the tree's server. Trusted daemon
provider/manager/control-plane networking is outside this workload boundary.
The one-use descriptor handoff socket is bind-mounted only into its helper;
ordinary workloads cannot connect to it or replace its pathname.

Gateway ownership outlives transient tool stacks while background workloads
remain, then retires after ten minutes without a holder. A stop,
policy retirement or restart closes the generation and its connections before
old authority can be reused. A gateway failure fails affected operations rather
than falling back to host networking. Raised shields retain public egress and
DNS but remove profile network exceptions.

The packet endpoint borrows a raw descriptor, so the gateway duplicates the
TUN descriptor atomically with close-on-exec and owns that copy. Retirement
detaches and waits for the packet dispatcher and application pumps before
closing it. If a stop deadline expires, a background waiter retains the copy
until those writers have stopped; closing it early could redirect packet I/O
into an unrelated file after the descriptor number is reused.

## Consequences

- Applications do not need HTTP/SOCKS configuration or custom TLS roots to use
  permitted networks.
- Host abstract Unix sockets are separated by the network namespace. Pathname
  socket limitations remain those explicitly accepted in ADR-0063.
- Networking introduces a stateful runtime owner and Go dependency, requiring
  compiled-process, lifecycle/recovery and packet-level verification.
- DNS, IPv6, UDP flow tracking and namespace/capability cleanup are security
  boundaries rather than optional compatibility details.
- Public internet access can transmit permitted data or reach an external
  relay. The gateway does not promise exfiltration prevention.
- Startup requires usable rootless namespace and TUN facilities. Unsupported
  installations fail explicitly instead of running a weaker sandbox.

## Alternatives Considered

- **Keep host networking while hiding socket paths.** Rejected because it
  preserves access to host services and abstract Unix sockets.
- **HTTP/SOCKS proxy environment variables only.** Rejected because clients can
  ignore them, and non-HTTP tooling needs individual integration.
- **TLS interception.** Rejected because certificate handling, client behavior
  and application inspection add complexity outside the required protection.
- **External networking helper.** Rejected in favor of a bundled Go library so
  users do not need another program alongside Bubblewrap.
- **Public-domain allowlists.** Rejected for the ordinary default: public
  internet remains available, with explicit exceptions for non-public services.
