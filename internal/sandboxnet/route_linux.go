//go:build linux

package sandboxnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"

	"github.com/miekg/dns"
	"golang.org/x/net/netutil"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// Router owns an immutable policy generation. Its namespace is never exported;
// process death releases every reference and the kernel destroys both veth pairs.
type Router struct {
	mu         sync.Mutex
	namespace  *os.File
	links      *routeLinks
	dns        *dnsService
	cutoff     bool
	done       chan struct{}
	stopErr    error
	stopOnce   sync.Once
	hostPolicy bool
}

// NewRouter attaches a daemon-created sandbox namespace to native Linux routing.
// It publishes the generation only after policy and DNS are ready.
func NewRouter(ctx context.Context, guestNS *os.File, cfg Config) (_ *Router, err error) {
	if err := checkHostForwarding(); err != nil {
		return nil, err
	}

	r := &Router{done: make(chan struct{})}

	r.namespace, err = newRouteNamespace()
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			err = errors.Join(err, r.Stop(context.WithoutCancel(ctx)))
		}
	}()

	r.links, err = openRouteLinks(int(r.namespace.Fd()), int(guestNS.Fd()))
	if err != nil {
		return nil, err
	}

	if err := configureRouterKernel(int(r.namespace.Fd())); err != nil {
		return nil, err
	}

	if err := r.links.create(int(guestNS.Fd()), cfg.Link, cfg.MTU); err != nil {
		return nil, err
	}

	if err := os.WriteFile("/proc/sys/net/ipv4/conf/"+r.links.name+"/route_localnet", []byte("1"), 0); err != nil {
		return nil, fmt.Errorf("enable granted host-loopback routing: %w", err)
	}

	services := r.links.loopbackServices(cfg.Link, cfg.Classifier)
	if !cfg.NoDNS {
		dnsServices, err := r.startDNS(ctx, cfg)
		if err != nil {
			return nil, err
		}

		services = append(services, dnsServices...)
	}

	rules, err := (routeRules{
		link: cfg.Link, classifier: cfg.Classifier, services: services,
		uplink4: r.links.router4, uplink6: r.links.router6,
		transit4: routeTransit4, transit6: routeTransit6,
	}).ruleset()
	if err != nil {
		return nil, err
	}
	// No child may retain an active router namespace. All configuration commands
	// finish while the veth links are down, before activation below.
	if err := applyRouteRules(ctx, int(r.namespace.Fd()), rules); err != nil {
		return nil, err
	}

	if err := applyRouteRules(ctx, -1, r.links.hostRules(cfg.Classifier)); err != nil {
		return nil, err
	}

	r.hostPolicy = true
	if err := r.links.activate(); err != nil {
		return nil, err
	}

	if err := r.links.confirmHostAddress(); err != nil {
		return nil, err
	}

	if err := r.links.installRoutes(cfg.Link); err != nil {
		return nil, err
	}

	return r, nil
}

// Cutoff revokes established traffic before callers wait for workload teardown.
// A failed cutoff retains ownership so retirement can be retried safely.
func (r *Router) Cutoff() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cutoff {
		return nil
	}

	if r.links != nil {
		if err := r.links.disconnect(); err != nil {
			return err
		}
	}

	r.cutoff = true

	return nil
}

// Stop disconnects immediately and joins resource cleanup within the deadline.
func (r *Router) Stop(ctx context.Context) error {
	if err := r.Cutoff(); err != nil {
		return err
	}
	// Cleanup outlives the caller's deadline: a generation half torn down would
	// leave links and rules behind with no owner to retry them.
	r.stopOnce.Do(func() { //nolint:contextcheck // Retirement deliberately outlives the caller's deadline.
		go func() {
			if r.dns != nil {
				r.dns.close()
			}

			if r.hostPolicy {
				r.stopErr = applyRouteRules(context.Background(), -1, deleteHostTable(r.links.name))
			}

			if r.links != nil {
				r.links.close()
			}

			r.stopErr = errors.Join(r.stopErr, r.namespace.Close())
			close(r.done)
		}()
	})

	select {
	case <-r.done:
		return r.stopErr
	case <-ctx.Done():
		return fmt.Errorf("wait for router cleanup: %w", ctx.Err())
	}
}

func (r *Router) startDNS(ctx context.Context, cfg Config) ([]routeService, error) {
	var err error

	r.dns, err = newDNSService(cfg.DNSUpstreams, cfg.DNSTimeout, maxInFlight(cfg.MaxInFlight))
	if err != nil {
		return nil, err
	}

	var (
		listen   net.ListenConfig
		services []routeService
	)

	for _, pair := range [][2]netip.Addr{{cfg.Link.GatewayIPv4, r.links.host4}, {cfg.Link.GatewayIPv6, r.links.host6}} {
		address := net.JoinHostPort(pair[1].String(), "0")

		packet, err := listen.ListenPacket(ctx, "udp", address)
		if err != nil {
			return nil, fmt.Errorf("bind routed DNS UDP: %w", err)
		}

		udpPort, err := boundPort(packet.LocalAddr())
		if err != nil {
			_ = packet.Close()

			return nil, err
		}

		if err := r.dns.startServer(&dns.Server{PacketConn: packet}); err != nil {
			_ = packet.Close()

			return nil, err
		}

		services = append(services, routeService{
			address: pair[0], target: pair[1], protocol: sandboxpolicy.ProtocolUDP,
			port: dnsPort, targetPort: udpPort,
		})

		listener, err := listen.Listen(ctx, "tcp", address)
		if err != nil {
			return nil, fmt.Errorf("bind routed DNS TCP: %w", err)
		}

		tcpPort, err := boundPort(listener.Addr())
		if err != nil {
			_ = listener.Close()

			return nil, err
		}

		if err := r.dns.startServer(
			&dns.Server{Listener: netutil.LimitListener(listener, maxInFlight(cfg.MaxInFlight))},
		); err != nil {
			_ = listener.Close()

			return nil, err
		}

		services = append(services, routeService{
			address: pair[0], target: pair[1], protocol: sandboxpolicy.ProtocolTCP,
			port: dnsPort, targetPort: tcpPort,
		})
	}

	return services, nil
}

// boundPort reads the ephemeral port the kernel assigned. The ruleset redirects
// to it, so an unreadable address must fail the generation, not go unnoticed.
func boundPort(address net.Addr) (int, error) {
	parsed, err := netip.ParseAddrPort(address.String())
	if err != nil {
		return 0, fmt.Errorf("read routed DNS port: %w", err)
	}

	return int(parsed.Port()), nil
}
