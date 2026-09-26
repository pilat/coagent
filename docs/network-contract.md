# Sandbox network contract

Sandbox traffic is routed by the Linux kernel. A network generation owns a
router namespace joined to the sandbox by one veth pair and to the host by
another; the kernel forwards, translates and connection-tracks, and no coagent
process sits in the packet path. This document describes the authority that
routing enforces and the evidence behind it.

## Authority

Public destinations are allowed. Host-attached networks, loopback, private and
special-use addresses require a matching protocol/port profile grant. ICMP is
granted per network in its own right and never implies a transport port. The
reserved host alias is translated only after its grant has been checked.

Decisions use actual destination addresses, never DNS answers or payloads. The
policy chain runs at prerouting priority `-150`, ahead of destination
translation, so no redirect can authorize a destination the policy refused. The
guest chain's default is drop: public egress is the explicit fall-through after
every private, host-attached, transit and router-owned prefix has been dropped.

| Contract | Evidence |
|---|---|
| Nothing the ruleset does not name is reachable; the guest, forward and input chains all default to drop. | `TestRuleset_DropsEveryDestinationItDoesNotName` |
| Private, loopback, link-local, host-attached, transit and router addresses are dropped without a grant, and public egress falls through. | `TestRuleset_PrivateAndHostNetworksAreDroppedWithoutAGrant` |
| A network grant opens exactly its protocol and port, and is read before the private-network drop. | `TestRuleset_NetworkGrantOpensOnlyItsProtocolAndPort` |
| An ICMP grant opens no transport port. | `TestRuleset_ICMPGrantDoesNotOpenTransportPorts` |
| The resolver is reachable only at the gateway address, and translation is ordered after the policy chain. | `TestRuleset_ResolverIsReachableOnlyAtTheGatewayAddress` |
| Without DNS nothing answers port 53 and no redirect exists. | `TestRuleset_WithoutDNSNothingAnswersPort53` |
| A service or grant that cannot be expressed as a rule fails generation creation instead of being skipped. | `TestRuleset_RejectsAnUnroutableService`, `TestRuleset_RejectsAnUnroutableGrant` |
| A grant is compiled from the destination the kernel delivered, including normalized IPv4-mapped IPv6. | `TestClassifier_*`, `TestConfigFromPolicy_*` |

## Transport behavior

TCP is one connection between the workload and its peer. Segmentation,
retransmission, options, urgent data, congestion control, error reporting and
path-MTU behavior are the kernel's. We neither reimplement nor test them; what
the tests check is that a generation actually carries traffic and stops it.

| Contract | Evidence |
|---|---|
| A generation carries TCP over IPv4 and IPv6 to a granted host-loopback service, in both directions and at size. | `TestNativeRouter_KernelChild` |
| TCP half-close lets the peer finish its direction. | `TestNativeRouter_KernelChild` |
| UDP preserves datagram boundaries and payloads, including empty datagrams and 65,507 bytes with fragmentation enabled. | `TestNativeRouter_KernelChild` |
| ICMP reaches public addresses; a private address without a grant does not answer. | `TestNativeRouter_KernelChild` |
| Cutoff stops established connections, not only new ones, before retirement waits for workloads. | `TestNativeRouter_KernelChild` |
| A joined sandbox process reaches the granted host alias and its own private loopback over the real namespace. | `TestNetworkJoinedRunner_AdmittedHostAliasAndPrivateLoopback` |
| The setup child holds a network namespace of its own and retires when its socket closes. | `TestNetworkSetup_HoldsItsOwnNamespaceUntilTheSocketCloses` |

## DNS

CoreDNS `forward` relays to the host's resolvers, captured when a generation
starts. Its listeners live in the host namespace and are reached only through
the generation's redirect of port 53 at the sandbox's gateway address. DNS TCP
framing and connection reuse follow
[RFC 7766 §6](https://www.rfc-editor.org/rfc/rfc7766.html#section-6).

| Contract | Evidence |
|---|---|
| Paired A/AAAA and repeated queries are all answered on one client socket. | `TestDNS_PairedQueriesShareUDPSocket`, and the namespace probe over the real link |
| TCP queries are answered with correct length framing across a reused connection. | `TestDNS_AnswersTCPWithLengthPrefix` |
| An unreachable upstream produces SERVFAIL rather than a hang. | `TestDNS_UnavailableUpstreamReturnsServfail` |
| Shutdown drains in-flight queries instead of abandoning them. | `TestDNS_ShutdownDrainsInFlightQueries` |
| Upstreams are validated as literal addresses; a hostname or invalid port fails generation creation. | `TestDNS_RejectsInvalidUpstreams`, `TestDNS_AcceptsScopedIPv6Resolver` |

## Limits

Source addresses are translated, so a peer sees the host's address rather than
the sandbox's. No sandbox port is published on the host; inbound connections
from outside reach nothing. Multicast and broadcast are not routed. Protocols
other than TCP, UDP and ICMP have no grant vocabulary and are dropped.

A generation captures the host's resolvers and interface addresses once; later
host changes reach the next generation, not a running one. TLS stays end to end:
there is no interception, no custom CA, no payload inspection and no
application-protocol analysis. Public egress remains public — the boundary is an
enumerated resource grant, not a promise about exfiltration.

The host must have IPv4 and IPv6 forwarding enabled and the daemon must hold
`CAP_NET_ADMIN` and `CAP_SYS_ADMIN`; a generation refuses to start otherwise
rather than running with a weaker boundary.

## Running the evidence

Ruleset, classifier and DNS scenarios run in the local suite. The real-namespace
scenarios are behind `//go:build integration` and run in CI, where the required
namespace privileges exist. Passing this matrix establishes these contracts; it
is not proof that every network behavior is covered. Add a regression scenario
whenever a new boundary case is found.
