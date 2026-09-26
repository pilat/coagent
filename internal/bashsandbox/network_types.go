package bashsandbox

import (
	"os"
	"time"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// EntryBinaryPath is the private sandbox path of the pinned daemon binary.
const EntryBinaryPath = sandboxpolicy.PrivateRuntimeDir + "/net-entry"

// NetworkLink holds the namespace and runtime files inherited by a joined command.
type NetworkLink struct {
	UserNS     *os.File
	NetNS      *os.File
	BinaryFile *os.File
	Resolver   *os.File
	Hosts      *os.File
}

// NetworkSetupConfig describes one namespace to create for a session tree.
type NetworkSetupConfig struct {
	Binary         string
	ReceiveTimeout time.Duration
}
