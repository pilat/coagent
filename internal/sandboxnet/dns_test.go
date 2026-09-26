//go:build linux

package sandboxnet

import (
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/netutil"
)

type fakeResolver struct {
	address string
	mu      sync.Mutex
	got     []uint16
}

func newFakeResolver(t *testing.T, handlers ...dns.HandlerFunc) *fakeResolver {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	require.NoError(t, err)
	r := &fakeResolver{address: tcp.Addr().String()}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
		r.mu.Lock()
		r.got = append(r.got, query.Question[0].Qtype)
		r.mu.Unlock()
		if len(handlers) > 0 {
			handlers[0](w, query)
			return
		}
		reply := new(dns.Msg).SetReply(query)
		reply.RecursionAvailable = true
		_ = w.WriteMsg(reply)
	})
	for _, server := range []*dns.Server{{Listener: tcp}, {PacketConn: udp}} {
		ready := make(chan struct{})
		server.Handler = handler
		server.NotifyStartedFunc = func() { close(ready) }
		finished := make(chan error, 1)
		go func() { finished <- server.ActivateAndServe() }()
		select {
		case <-ready:
		case err := <-finished:
			t.Fatalf("start fake resolver: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("fake resolver did not start")
		}
		t.Cleanup(func() {
			require.NoError(t, server.Shutdown())
			require.NoError(t, <-finished)
		})
	}
	return r
}

func (r *fakeResolver) queries() []uint16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint16(nil), r.got...)
}

// routedResolver starts the generation's DNS service on loopback, the way the
// router binds it on the host side of the uplink before redirecting port 53.
func routedResolver(t *testing.T, upstreams []string) (*dnsService, string, string) {
	t.Helper()
	service, err := newDNSService(upstreams, 0, maxInFlight(0))
	require.NoError(t, err)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			service.close()
		}
	})
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, service.startServer(&dns.Server{PacketConn: packet}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, service.startServer(&dns.Server{Listener: netutil.LimitListener(listener, maxInFlight(0))}))
	t.Cleanup(func() { stopped = true })
	return service, packet.LocalAddr().String(), listener.Addr().String()
}

func dialResolver(t *testing.T, network, address string) *dns.Conn {
	t.Helper()
	conn, err := net.Dial(network, address)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	return &dns.Conn{Conn: conn}
}

func TestDNS_AnswersFromConfiguredUpstream(t *testing.T) {
	resolver := newFakeResolver(t)
	_, udpAddress, _ := routedResolver(t, []string{resolver.address})
	client := dialResolver(t, "udp", udpAddress)
	query := new(dns.Msg).SetQuestion("example.test.", dns.TypeA)
	require.NoError(t, client.WriteMsg(query))
	reply, err := client.ReadMsg()
	require.NoError(t, err)
	assert.Equal(t, query.Id, reply.Id)
	assert.Equal(t, dns.RcodeSuccess, reply.Rcode)
	assert.Equal(t, []uint16{dns.TypeA}, resolver.queries())
}

// A system resolver sends A and AAAA together on one socket; answering only the
// first made every lookup wait for the client's retry.
func TestDNS_PairedQueriesShareUDPSocket(t *testing.T) {
	resolver := newFakeResolver(t)
	_, udpAddress, _ := routedResolver(t, []string{resolver.address})
	client := dialResolver(t, "udp", udpAddress)
	for round := range 3 {
		for index, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
			query := new(dns.Msg).SetQuestion("example.test.", qtype)
			query.Id = uint16(round*2 + index + 1)
			require.NoError(t, client.WriteMsg(query))
		}
		var ids []uint16
		for range 2 {
			reply, err := client.ReadMsg()
			require.NoError(t, err, "both answers must arrive before resolver retry")
			ids = append(ids, reply.Id)
		}
		assert.ElementsMatch(t, []uint16{uint16(round*2 + 1), uint16(round*2 + 2)}, ids)
	}
}

func TestDNS_AnswersTCPWithLengthPrefix(t *testing.T) {
	resolver := newFakeResolver(t)
	_, _, tcpAddress := routedResolver(t, []string{resolver.address})
	client := dialResolver(t, "tcp", tcpAddress)
	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeTXT} {
		query := new(dns.Msg).SetQuestion("example.test.", qtype)
		require.NoError(t, client.WriteMsg(query))
		reply, err := client.ReadMsg()
		require.NoError(t, err)
		assert.Equal(t, query.Id, reply.Id)
	}
	assert.Equal(t, []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeTXT}, resolver.queries())
}

func TestDNS_ShutdownDrainsInFlightQueries(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	resolver := newFakeResolver(t, func(w dns.ResponseWriter, query *dns.Msg) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		_ = w.WriteMsg(new(dns.Msg).SetReply(query))
	})
	service, udpAddress, _ := routedResolver(t, []string{resolver.address})
	client := dialResolver(t, "udp", udpAddress)
	require.NoError(t, client.WriteMsg(new(dns.Msg).SetQuestion("example.test.", dns.TypeA)))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("query did not reach upstream")
	}
	closed := make(chan struct{})
	go func() {
		service.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("shutdown abandoned an in-flight query")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the upstream replied")
	}
	runtime.GC()
}

func TestDNS_UnavailableUpstreamReturnsServfail(t *testing.T) {
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.LocalAddr().String()
	require.NoError(t, listener.Close())
	_, udpAddress, _ := routedResolver(t, []string{address})
	client := dialResolver(t, "udp", udpAddress)
	require.NoError(t, client.WriteMsg(new(dns.Msg).SetQuestion("example.test.", dns.TypeA)))
	reply, err := client.ReadMsg()
	require.NoError(t, err)
	assert.Equal(t, dns.RcodeServerFailure, reply.Rcode)
}

func TestDNS_AcceptsScopedIPv6Resolver(t *testing.T) {
	service, err := newDNSService([]string{"[fe80::1%eth0]:53"}, 0, maxInFlight(0))
	require.NoError(t, err)
	service.close()
}

func TestDNS_RejectsInvalidUpstreams(t *testing.T) {
	for _, upstreams := range [][]string{{}, {"resolver.example:53"}, {"127.0.0.1:0"}, {"127.0.0.1:65536"}} {
		service, err := newDNSService(upstreams, 0, maxInFlight(0))
		require.Error(t, err)
		assert.Nil(t, service)
	}
}
