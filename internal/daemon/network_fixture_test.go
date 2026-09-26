package daemon

import (
	"context"
	"errors"
	"net"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// Daemon protocol fixtures leave packet admission to the compiled-process tests.
type (
	fixtureNetworkOwner struct{}
	fixtureNetworkLease struct{}
)

func (fixtureNetworkOwner) Acquire(context.Context, int64, sandboxpolicy.Policy) (builtin.NetworkLease, error) {
	return fixtureNetworkLease{}, nil
}

func (fixtureNetworkLease) Link() *bashsandbox.NetworkLink     { return nil }
func (fixtureNetworkLease) BindRunner(procexec.Runner, string) {}
func (fixtureNetworkLease) Release()                           {}
func (fixtureNetworkLease) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("network unavailable in daemon fixture")
}
