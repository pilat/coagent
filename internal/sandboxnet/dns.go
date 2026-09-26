//go:build linux

package sandboxnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin/forward"
	"github.com/coredns/coredns/plugin/pkg/proxy"
	"github.com/miekg/dns"
)

const (
	dnsPort       = 53
	dnsTimeout    = 5 * time.Second
	dnsMaxMessage = dns.MaxMsgSize

	// defaultMaxInFlight bounds concurrent resolver requests and TCP clients.
	defaultMaxInFlight = 256
)

// maxInFlight applies the default when the configuration leaves it unset.
func maxInFlight(configured int) int {
	if configured <= 0 {
		return defaultMaxInFlight
	}

	return configured
}

type dnsService struct {
	forward *forward.Forward
	servers []*dns.Server
	ctx     context.Context //nolint:containedctx // DNS requests share the generation lifetime, ended by close.
	cancel  context.CancelFunc
	slots   chan struct{}
	wg      sync.WaitGroup
	timeout time.Duration
}

func (d *dnsService) ServeDNS(writer dns.ResponseWriter, query *dns.Msg) {
	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	default:
		_ = writer.WriteMsg(new(dns.Msg).SetRcode(query, dns.RcodeRefused))
		return
	}

	ctx, cancel := context.WithTimeout(d.ctx, d.timeout)
	defer cancel()

	code, err := d.forward.ServeDNS(ctx, writer, query)
	if err != nil || code != dns.RcodeSuccess {
		if code == dns.RcodeSuccess {
			code = dns.RcodeServerFailure
		}

		_ = writer.WriteMsg(new(dns.Msg).SetRcode(query, code))
	}
}

func systemResolvers() ([]string, error) {
	config, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("read host DNS configuration: %w", err)
	}

	servers := make([]string, 0, len(config.Servers))
	for _, address := range config.Servers {
		servers = append(servers, net.JoinHostPort(address, config.Port))
	}

	return servers, nil
}

func newDNSService(upstreams []string, timeout time.Duration, maxFlows int) (*dnsService, error) {
	if upstreams == nil {
		var err error

		upstreams, err = systemResolvers()
		if err != nil {
			return nil, err
		}
	}

	if len(upstreams) == 0 {
		return nil, errors.New("no host DNS resolver configured")
	}

	if timeout <= 0 {
		timeout = dnsTimeout
	}

	if err := validateDNSUpstreams(upstreams); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := &dnsService{
		forward: forward.New(), ctx: ctx, cancel: cancel,
		slots: make(chan struct{}, maxFlows), timeout: timeout,
	}

	for _, address := range upstreams {
		p := proxy.NewProxy("forward", address, "dns")
		// Generation shutdown owns the transport; its finalizer would stop it twice.
		runtime.SetFinalizer(p, nil)
		p.SetExpire(10 * time.Second)
		p.SetMaxIdleConns(maxFlows)
		p.SetReadTimeout(min(timeout, 2*time.Second))
		d.forward.SetProxy(p)
	}

	return d, nil
}

func (d *dnsService) startServer(server *dns.Server) error {
	ready := make(chan struct{})
	result := make(chan error, 1)
	server.Handler = d
	server.UDPSize = dnsMaxMessage
	server.ReadTimeout = d.timeout
	server.WriteTimeout = d.timeout
	server.IdleTimeout = func() time.Duration { return d.timeout }
	server.NotifyStartedFunc = func() { close(ready) }

	d.wg.Go(func() { result <- server.ActivateAndServe() })

	select {
	case <-ready:
		d.servers = append(d.servers, server)
		return nil
	case err := <-result:
		return fmt.Errorf("start virtual DNS server: %w", err)
	}
}

// Upstreams are trusted configuration, never destinations supplied by a query.
func validateDNSUpstreams(upstreams []string) error {
	for _, address := range upstreams {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("invalid DNS upstream %q: %w", address, err)
		}

		if _, err := netip.ParseAddr(host); err != nil {
			return fmt.Errorf("invalid DNS upstream %q", address)
		}

		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("invalid DNS upstream port %q", port)
		}
	}

	return nil
}

func (d *dnsService) close() {
	d.cancel()

	var stops sync.WaitGroup
	for _, server := range d.servers {
		stops.Go(func() { _ = server.Shutdown() })
	}

	stops.Wait()
	d.wg.Wait()
	_ = d.forward.OnShutdown()
	// Proxy Stop ends health checks; transport caches have their own lifetime.
	for _, p := range d.forward.List() {
		p.GetTransport().Stop()
	}
}
