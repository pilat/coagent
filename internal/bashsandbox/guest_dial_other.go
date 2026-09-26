//go:build !linux

package bashsandbox

import (
	"context"
	"errors"
	"net"

	"github.com/pilat/coagent/internal/procexec"
)

const DialCommand = "__coagent_net_dial"

func RunDialMode(args []string) (bool, error) {
	if len(args) == 0 || args[0] != DialCommand {
		return false, nil
	}
	return true, errors.New("guest network dial requires Linux")
}

func DialGuest(context.Context, procexec.Runner, string, string, string, string) (net.Conn, error) {
	return nil, errors.New("guest network dial requires Linux")
}
