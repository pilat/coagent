//go:build linux && integration

package sandboxnet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func TestNativeRouter_IsolatedKernel(t *testing.T) {
	if os.Getenv("COAGENT_ROUTE_FIXTURE") == "1" {
		t.Skip("outer test only")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	//nolint:gosec // Fixed argv; re-invokes this test binary.
	command := exec.CommandContext(ctx, "unshare", "--user", "--map-root-user", "--net", "--", os.Args[0],
		"-test.run=^TestNativeRouter_KernelChild$", "-test.v", "-test.timeout=30s")
	command.Env = append(os.Environ(), "COAGENT_ROUTE_FIXTURE=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Log(string(output))
}

func TestNativeRouter_KernelChild(t *testing.T) {
	if os.Getenv("COAGENT_ROUTE_FIXTURE") != "1" {
		t.Skip("requires isolated outer network namespace")
	}
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0))
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0))
	// Operators do disable IPv6 for new interfaces; the transit veth must still
	// take its addresses. Existing links keep the setting they were created with.
	require.NoError(t, os.WriteFile("/proc/sys/net/ipv6/conf/default/disable_ipv6", []byte("1"), 0))
	host, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	require.NoError(t, err)
	defer host.Close()
	lo, err := host.LinkByName("lo")
	require.NoError(t, err)
	require.NoError(t, host.LinkSetUp(lo))
	for _, address := range []string{"93.184.216.34/32", "2606:2800:220:1::34/128", "10.99.0.1/32"} {
		parsed, err := netlink.ParseAddr(address)
		require.NoError(t, err)
		parsed.Flags = unix.IFA_F_NODAD
		require.NoError(t, host.AddrAdd(lo, parsed))
	}
	guestNS, err := newRouteNamespace()
	require.NoError(t, err)
	defer func() { _ = guestNS.Close() }()
	guest, err := netlink.NewHandleAt(netns.NsHandle(guestNS.Fd()), unix.NETLINK_ROUTE)
	require.NoError(t, err)
	defer guest.Close()
	guestLoopback, err := guest.LinkByName("lo")
	require.NoError(t, err)
	require.NoError(t, guest.LinkSetUp(guestLoopback))
	var grants []sandboxpolicy.Network
	var listeners []net.Listener
	var packets []net.PacketConn
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		packet, err := net.ListenPacket("udp", address)
		require.NoError(t, err)
		defer func() { _ = packet.Close() }()
		packets = append(packets, packet)
		grants = append(grants, sandboxpolicy.Network{
			Address:  sandboxpolicy.HostLoopback,
			Protocol: sandboxpolicy.ProtocolUDP, Ports: []int{packet.LocalAddr().(*net.UDPAddr).Port},
		})
		go func() {
			buffer := make([]byte, 65535)
			for {
				n, peer, err := packet.ReadFrom(buffer)
				if err != nil {
					return
				}
				_, _ = packet.WriteTo(buffer[:n], peer)
			}
		}()
		listener, err := net.Listen("tcp", address)
		require.NoError(t, err)
		defer func() { _ = listener.Close() }()
		listeners = append(listeners, listener)
		grants = append(grants, sandboxpolicy.Network{
			Address:  sandboxpolicy.HostLoopback,
			Protocol: sandboxpolicy.ProtocolTCP, Ports: []int{listener.Addr().(*net.TCPAddr).Port},
		})
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() { defer func() { _ = conn.Close() }(); _, _ = io.Copy(conn, conn) }()
			}
		}()
	}
	classifier, err := NewClassifier(nil, grants)
	require.NoError(t, err)
	resolver := newFakeResolver(t)
	router, err := NewRouter(
		t.Context(),
		guestNS,
		Config{Link: DefaultLink(), Classifier: classifier, DNSUpstreams: []string{resolver.address}},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, router.Stop(context.Background())) })
	for _, address := range []string{"93.184.216.34", "2606:2800:220:1::34"} {
		require.NoError(t, inRouteNamespace(int(guestNS.Fd()), func() error {
			output, err := exec.CommandContext(t.Context(), "ping", "-n", "-c", "1", "-W", "1", address).
				CombinedOutput()
			if err != nil {
				return fmt.Errorf("ping %s: %w: %s", address, err, output)
			}
			return nil
		}))
	}
	require.Error(t, inRouteNamespace(int(guestNS.Fd()), func() error {
		return exec.CommandContext(t.Context(), "ping", "-n", "-c", "1", "-W", "1", "10.99.0.1").Run()
	}), "private ICMP requires a grant")
	var active []net.Conn
	for i, packet := range packets {
		address := DefaultLink().HostAliasIPv4
		gateway := DefaultLink().GatewayIPv4
		if i == 1 {
			address, gateway = DefaultLink().HostAliasIPv6, DefaultLink().GatewayIPv6
		}
		var conn net.Conn
		require.NoError(t, inRouteNamespace(int(guestNS.Fd()), func() error {
			var err error
			conn, err = net.DialTimeout(
				"udp",
				net.JoinHostPort(address.String(), strconv.Itoa(packet.LocalAddr().(*net.UDPAddr).Port)),
				time.Second,
			)
			return err
		}))
		defer func() { _ = conn.Close() }()
		raw, err := conn.(*net.UDPConn).SyscallConn()
		require.NoError(t, err)
		require.NoError(t, raw.Control(func(fd uintptr) {
			if i == 0 {
				err = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_DONT)
			}
		}))
		require.NoError(t, err)
		for _, size := range []int{0, 1, 1472, 32769, 60000, 65507} {
			require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
			payload := bytes.Repeat([]byte{byte(size)}, size)
			_, err := conn.Write(payload)
			require.NoError(t, err)
			buffer := make([]byte, 65535)
			n, err := conn.Read(buffer)
			require.NoError(t, err)
			require.Equal(t, payload, buffer[:n])
		}
		active = append(active, conn)
		var dnsConn net.Conn
		require.NoError(t, inRouteNamespace(int(guestNS.Fd()), func() error {
			var err error
			dnsConn, err = net.DialTimeout("udp", net.JoinHostPort(gateway.String(), "53"), time.Second)
			return err
		}))
		defer func() { _ = dnsConn.Close() }()
		require.NoError(t, dnsConn.SetDeadline(time.Now().Add(time.Second)))
		wire := &dns.Conn{Conn: dnsConn}
		for _, kind := range []uint16{dns.TypeA, dns.TypeAAAA} {
			require.NoError(t, wire.WriteMsg(new(dns.Msg).SetQuestion("example.test.", kind)))
		}
		for range 2 {
			answer, err := wire.ReadMsg()
			require.NoError(t, err)
			require.Equal(t, dns.RcodeSuccess, answer.Rcode)
		}
	}
	for i, listener := range listeners {
		address := DefaultLink().HostAliasIPv4
		if i == 1 {
			address = DefaultLink().HostAliasIPv6
		}
		_, port, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, err)
		var conn net.Conn
		require.NoError(t, inRouteNamespace(int(guestNS.Fd()), func() error {
			var err error
			conn, err = net.DialTimeout("tcp", net.JoinHostPort(address.String(), port), time.Second)
			return err
		}))
		defer func() { _ = conn.Close() }()
		require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
		_, err = io.WriteString(conn, strings.Repeat("routed", 1000))
		require.NoError(t, err)
		payload := make([]byte, 6000)
		_, err = io.ReadFull(conn, payload)
		require.NoError(t, err)
		require.Equal(t, strings.Repeat("routed", 1000), string(payload))
		active = append(active, conn)
	}
	for _, conn := range active {
		if _, ok := conn.(*net.TCPConn); ok {
			var half net.Conn
			require.NoError(t, inRouteNamespace(int(guestNS.Fd()), func() error {
				var err error
				half, err = net.DialTimeout("tcp", conn.RemoteAddr().String(), time.Second)
				return err
			}))
			tcp := half.(*net.TCPConn)
			defer func() { _ = tcp.Close() }()
			require.NoError(t, tcp.SetDeadline(time.Now().Add(time.Second)))
			_, err := io.WriteString(tcp, "half-close")
			require.NoError(t, err)
			require.NoError(t, tcp.CloseWrite())
			response, err := io.ReadAll(tcp)
			require.NoError(t, err, "half-close must let the peer finish")
			require.Equal(t, "half-close", string(response))
		}
	}
	// A host udev policy rewriting the uplink MAC strands the pinned neighbour,
	// and the kernel then drops the frames before any firewall hook sees them.
	// Creating the link with our own address is what prevents it; this guard is
	// what turns a later rewrite into an error instead of a silent black hole.
	require.NotEmpty(t, router.links.hostMAC)
	require.Equal(
		t,
		byte(0x02),
		router.links.hostMAC[0]&0x03,
		"the uplink address must be locally administered unicast",
	)
	require.NoError(t, router.links.confirmHostAddress())

	hostLink, err := router.links.host.LinkByName(router.links.name)
	require.NoError(t, err)
	require.NoError(t, router.links.host.LinkSetDown(hostLink))
	require.NoError(t, router.links.host.LinkSetHardwareAddr(hostLink, net.HardwareAddr{0x02, 0, 0, 0, 0, 0xff}))
	require.NoError(t, router.links.host.LinkSetUp(hostLink))
	require.Error(t, router.links.confirmHostAddress(), "a rewritten uplink address must be reported, not ignored")

	require.NoError(t, router.Cutoff())
	for _, conn := range active {
		require.NoError(t, conn.SetDeadline(time.Now().Add(100*time.Millisecond)))
		_, _ = io.WriteString(conn, "revoked")
		buffer := make([]byte, 1)
		_, err := conn.Read(buffer)
		require.Error(t, err, "established traffic must stop before retirement completes")
	}
}
