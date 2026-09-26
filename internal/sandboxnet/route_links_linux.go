//go:build linux

package sandboxnet

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

var (
	routeTransit4 = netip.MustParsePrefix("10.212.0.0/16")
	routeTransit6 = netip.MustParsePrefix("fd00:63:6f:62::/64")
)

type routeLinks struct {
	host         *netlink.Handle
	router       *netlink.Handle
	guest        *netlink.Handle
	hostLink     netlink.Link
	uplink       netlink.Link
	guestLink    netlink.Link
	workloadLink netlink.Link
	host4        netip.Addr
	host6        netip.Addr
	router4      netip.Addr
	router6      netip.Addr
	name         string
	hostMAC      net.HardwareAddr
}

func openRouteLinks(routerFD, guestFD int) (_ *routeLinks, err error) {
	l := &routeLinks{}

	defer func() {
		if err != nil {
			l.close()
		}
	}()

	l.host, err = netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("open host route netlink: %w", err)
	}

	l.router, err = netlink.NewHandleAt(netns.NsHandle(routerFD), unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("open router netlink: %w", err)
	}

	l.guest, err = netlink.NewHandleAt(netns.NsHandle(guestFD), unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("open sandbox netlink: %w", err)
	}

	for _, h := range []*netlink.Handle{l.host, l.router, l.guest} {
		if err := h.SetSocketTimeout(routeCommandTimeout); err != nil {
			return nil, fmt.Errorf("bound route netlink requests: %w", err)
		}
	}

	if err := l.allocate(); err != nil {
		return nil, err
	}

	return l, nil
}

func (l *routeLinks) allocate() error {
	routes, err := l.host.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("inspect transit routes: %w", err)
	}

	for range 128 {
		var entropy [14]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return fmt.Errorf("allocate route identity: %w", err)
		}

		slot := uint32(binary.BigEndian.Uint16(entropy[:2])&0x3fff) * 4
		v4 := [4]byte{10, 212, byte(slot >> 8), byte(slot)}
		prefix := netip.PrefixFrom(netip.AddrFrom4(v4), 30)
		occupied := false

		for _, route := range routes {
			if route.Dst == nil {
				continue
			}

			p, err := netip.ParsePrefix(route.Dst.String())
			if err == nil && p.Bits() != 0 && p.Overlaps(prefix) {
				occupied = true
				break
			}
		}

		if occupied {
			continue
		}

		v6 := routeTransit6.Addr().As16()
		binary.BigEndian.PutUint32(v6[12:], slot)
		l.host4, l.host6 = prefix.Addr().Next(), netip.AddrFrom16(v6).Next()
		l.router4, l.router6 = l.host4.Next(), l.host6.Next()
		l.name = fmt.Sprintf("cag%x", entropy[2:8])
		// Locally administered unicast: the host end must carry an address we
		// chose, not one the kernel randomized (see create).
		l.hostMAC = net.HardwareAddr{
			entropy[8]&^0x01 | 0x02,
			entropy[9],
			entropy[10],
			entropy[11],
			entropy[12],
			entropy[13],
		}

		return nil
	}

	return fmt.Errorf("no free sandbox transit subnet in %s", routeTransit4)
}

func (l *routeLinks) create(guestFD int, link LinkAddresses, mtu uint32) error {
	if mtu == 0 {
		mtu = 1500
	}

	// The host end must be created with its address already set. A host udev
	// policy such as MACAddressPolicy=persistent rewrites a kernel-randomized
	// MAC moments later, stranding the permanent neighbour pinned below.
	if err := l.router.LinkAdd(&netlink.Veth{
		Name: routerUplink, MTU: int(mtu),
		PeerName: l.name, PeerHardwareAddr: l.hostMAC,
	}); err != nil {
		return fmt.Errorf("create router uplink: %w", err)
	}
	// Move the peer through a pinned host namespace, never a process identifier.
	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("pin host namespace: %w", err)
	}
	defer func() { _ = hostNS.Close() }()

	peer, err := l.router.LinkByName(l.name)
	if err != nil {
		return fmt.Errorf("find uplink peer: %w", err)
	}

	if err := l.router.LinkSetNsFd(peer, int(hostNS)); err != nil {
		return fmt.Errorf("attach uplink to host: %w", err)
	}

	l.hostLink, err = l.host.LinkByName(l.name)
	if err != nil {
		return fmt.Errorf("find host uplink: %w", err)
	}
	// A host that disables IPv6 for new interfaces hands us a veth that refuses
	// addresses. This transit link is ours, not the operator's network.
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+l.name+"/disable_ipv6", []byte("0"), 0); err != nil {
		return fmt.Errorf("enable transit IPv6 on %s: %w", l.name, err)
	}

	l.uplink, err = l.router.LinkByName(routerUplink)
	if err != nil {
		return fmt.Errorf("find router uplink: %w", err)
	}

	if err := l.router.LinkAdd(&netlink.Veth{
		Name: routerGuestLink, MTU: int(mtu),
		PeerName: SetupName, PeerNamespace: netlink.NsFd(guestFD),
	}); err != nil {
		return fmt.Errorf("create sandbox link: %w", err)
	}

	l.guestLink, err = l.router.LinkByName(routerGuestLink)
	if err != nil {
		return fmt.Errorf("find sandbox router link: %w", err)
	}

	l.workloadLink, err = l.guest.LinkByName(SetupName)
	if err != nil {
		return fmt.Errorf("find sandbox workload link: %w", err)
	}

	if err := l.address(link); err != nil {
		return err
	}

	return l.pinNeighbors(link)
}

// address assigns both ends of each veth. pinNeighbors then fixes the peers, so
// neither namespace has to discover the other.
func (l *routeLinks) address(link LinkAddresses) error {
	for _, assignment := range []struct {
		h       *netlink.Handle
		device  netlink.Link
		address netip.Addr
		bits    int
	}{
		{l.host, l.hostLink, l.host4, 30},
		{l.host, l.hostLink, l.host6, 126},
		{l.router, l.uplink, l.router4, 30},
		{l.router, l.uplink, l.router6, 126},
		{l.router, l.guestLink, link.GatewayIPv4, 29},
		{l.router, l.guestLink, link.GatewayIPv6, 125},
		{l.router, l.guestLink, link.HostAliasIPv4, 32},
		{l.router, l.guestLink, link.HostAliasIPv6, 128},
		{l.guest, l.workloadLink, link.SandboxIPv4, 29},
		{l.guest, l.workloadLink, link.SandboxIPv6, 125},
	} {
		address := &netlink.Addr{
			IPNet: &net.IPNet{
				IP:   net.IP(assignment.address.AsSlice()),
				Mask: net.CIDRMask(assignment.bits, assignment.address.BitLen()),
			},
			Flags: unix.IFA_F_NODAD,
		}
		if err := assignment.h.AddrAdd(assignment.device, address); err != nil {
			return fmt.Errorf("address %s: %w", assignment.address, err)
		}
	}

	return nil
}

func (l *routeLinks) pinNeighbors(link LinkAddresses) error {
	for _, neighbor := range []struct {
		h         *netlink.Handle
		device    netlink.Link
		peer      netlink.Link
		addresses []netip.Addr
	}{
		{l.host, l.hostLink, l.uplink, []netip.Addr{l.router4, l.router6}},
		{l.router, l.uplink, l.hostLink, []netip.Addr{l.host4, l.host6}},
		{l.router, l.guestLink, l.workloadLink, []netip.Addr{link.SandboxIPv4, link.SandboxIPv6}},
		{l.guest, l.workloadLink, l.guestLink, []netip.Addr{link.GatewayIPv4, link.GatewayIPv6, link.HostAliasIPv4, link.HostAliasIPv6}},
	} {
		for _, address := range neighbor.addresses {
			if err := neighbor.h.NeighSet(&netlink.Neigh{
				LinkIndex: neighbor.device.Attrs().Index, State: netlink.NUD_PERMANENT,
				IP: net.IP(address.AsSlice()), HardwareAddr: neighbor.peer.Attrs().HardwareAddr,
			}); err != nil {
				return fmt.Errorf("set isolated peer %s: %w", address, err)
			}
		}
	}

	return nil
}

// confirmHostAddress catches a host policy that rewrites the uplink address
// after creation. The pinned neighbour would otherwise send every packet to an
// address nobody answers, and the kernel drops those before any firewall hook.
func (l *routeLinks) confirmHostAddress() error {
	link, err := l.host.LinkByName(l.name)
	if err != nil {
		return fmt.Errorf("re-read host uplink: %w", err)
	}

	if got := link.Attrs().HardwareAddr; !bytes.Equal(got, l.hostMAC) {
		return fmt.Errorf("host uplink %s address became %s instead of %s", l.name, got, l.hostMAC)
	}

	return nil
}

func (l *routeLinks) installRoutes(link LinkAddresses) error {
	for _, route := range []struct {
		h       *netlink.Handle
		device  netlink.Link
		gateway netip.Addr
	}{
		{l.router, l.uplink, l.host4},
		{l.router, l.uplink, l.host6},
		{l.guest, l.workloadLink, link.GatewayIPv4},
		{l.guest, l.workloadLink, link.GatewayIPv6},
	} {
		if err := route.h.RouteAdd(
			&netlink.Route{LinkIndex: route.device.Attrs().Index, Gw: net.IP(route.gateway.AsSlice())},
		); err != nil {
			return fmt.Errorf("route through %s: %w", route.gateway, err)
		}
	}

	return nil
}

func (l *routeLinks) activate() error {
	for _, item := range []struct {
		h    *netlink.Handle
		link netlink.Link
	}{
		{l.host, l.hostLink},
		{l.guest, l.workloadLink},
		{l.router, l.guestLink},
		{l.router, l.uplink},
	} {
		if err := item.h.LinkSetUp(item.link); err != nil {
			return fmt.Errorf("activate route link %s: %w", item.link.Attrs().Name, err)
		}
	}

	return nil
}

func (l *routeLinks) disconnect() error {
	if l.guestLink == nil {
		return nil
	}

	if err := l.router.LinkSetDown(l.guestLink); err != nil {
		return fmt.Errorf("disconnect sandbox route: %w", err)
	}

	return nil
}

func (l *routeLinks) close() {
	for _, handle := range []*netlink.Handle{l.guest, l.router, l.host} {
		if handle != nil {
			handle.Close()
		}
	}
}
