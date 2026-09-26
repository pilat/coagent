# ADR-0069: Route sandbox traffic through the Linux kernel

- **Status:** Accepted
- **Date:** 2026-09-25

## Context

[ADR-0064](0064-embedded-transparent-sandbox-gateway.md) put a userspace TCP/IP
stack between the sandbox and the host: the gateway terminated the guest's TCP
connection and opened a second one from the host. Bytes copied between them, so
the two connections were only as alike as our copying made them.

That gap was not theoretical. Connection-refused and unreachable outcomes
collapsed into a generic reset, TCP options and urgent data did not survive the
hop, ICMP had no path at all, and every transport property needed its own test
and its own reimplementation. The operator's requirement was an ordinary network
with access control and a kill switch, not a protocol reimplementation.

The kernel already routes, filters, translates and tracks connections. Using it
requires privileges the daemon did not previously hold, which was the reason to
avoid it.

## Decision

We route sandbox traffic with the Linux kernel and describe authority in
nftables. A generation owns a router network namespace connected to the sandbox
namespace by one veth pair and to the host by another. The kernel forwards,
NATs and connection-tracks; no coagent process is in the packet path.

The daemon configures this itself, with `CAP_NET_ADMIN` and `CAP_SYS_ADMIN`
granted by its service unit. We considered a separate privileged helper and
rejected it: coagent is already the trusted orchestrator of the sandbox, and a
helper would move the same authority without reducing what a compromised daemon
could reach. The privilege stops at the daemon — every workload it launches is
re-exec'd through a launcher that clears ambient and inheritable capabilities,
and the sandbox entry stage additionally empties its bounding set before exec.

Authority lives in one ruleset applied while the links are still down. The guest
chain drops by default; public destinations fall through to a final accept, and
host, private and special-use prefixes are dropped unless a profile grant names
their protocol and port. The policy chain runs before destination translation,
so a redirect can never authorize a destination the policy refused. ICMP is a
grantable protocol in its own right and never implies a transport port.

`host.coagent.internal` reaches the daemon's loopback only for granted
protocol/port pairs, translated on the host side of the uplink. IPv6 needs one
extra step there: strict route lookup rejects `::1` arriving on a veth, so the
granted connection is delivered under the uplink address and restored to `::1`
after local delivery is chosen, following the approach Netfilter's maintainers
describe for this case.

Cutoff takes the guest link down, which stops established connections too, and
it happens before the generation waits for workloads to exit. A cutoff that
fails retains ownership rather than releasing the barrier, so the next
generation cannot start over a network that is still up.

The host must have IPv4 and IPv6 forwarding enabled. A generation refuses to
start otherwise instead of running with a weaker boundary.

## Consequences

- TCP is one connection end to end. Errors, options, timing and flow control are
  the kernel's, not our approximation of them.
- ICMP works, including `ping` to public addresses, and it is grantable per
  network like any other protocol.
- Transport correctness is no longer ours to test. The verification that remains
  is about authority — which ruleset we generate — plus real-namespace checks
  that the generation comes up, carries traffic and shuts off on cutoff.
- The daemon holds network-administration capabilities. Compromising it means
  compromising the host's network configuration; the sandbox boundary now rests
  on the daemon staying uncompromised, and on every spawn path dropping
  capabilities before user code runs.
- Installation gains two prerequisites: host forwarding and a service unit with
  ambient capabilities. Both fail loudly.
- gVisor leaves the dependency graph, along with the TUN device, the userspace
  stack and its transport test matrix.

## Alternatives Considered

- **Keep the userspace stack and add ICMP to it.** Rejected: it fixes one
  symptom and leaves every other transport difference, each needing its own
  implementation and its own regression test.
- **A separate root-owned network helper process.** Rejected: coagent already
  orchestrates the sandbox, so the helper relocates the authority without
  shrinking it, at the cost of an installation step and an IPC protocol.
- **Pre-configured host networking, no runtime privilege.** Rejected: a
  generation's addressing and rules are per policy and per session tree, so a
  static host configuration cannot express them.
- **NFQUEUE for every packet.** Rejected for now: the current authority is
  expressible as rules, and a userspace verdict on each packet would reintroduce
  a process in the packet path. The ruleset leaves room to queue selectively
  when protocol analysis is actually needed.
- **A migration prototype alongside the gateway.** Rejected: two network paths
  would double the lifecycle surface — generations, holders, retirement — for a
  boundary that must have exactly one answer.
