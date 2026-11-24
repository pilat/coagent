package localapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

const statusTestVersion = "0.4.2"

var statusTestConfigPath = "~/" + coagenthome.DirName + "/" + coagenthome.ConfigFileName

type fakeManagers struct {
	mu      sync.Mutex
	running []string
	errs    map[string]error
}

type statusHarness struct {
	socket   string
	server   *Server
	client   coagentv1connect.DaemonServiceClient
	managers *fakeManagers
}

func (f *fakeManagers) RunningIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.running
}

func (f *fakeManagers) StartError(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.errs[id]
}

func (f *fakeManagers) fail(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.errs == nil {
		f.errs = map[string]error{}
	}

	f.errs[id] = err
}

func newStatusHarness(t *testing.T, cfg *config.Config, ready bool) *statusHarness {
	t.Helper()

	listener, socket := apiListener(t)
	managers := &fakeManagers{}
	server := New(t.Context(), listener, StatusSource{
		Config: cfg, ConfigPath: statusTestConfigPath, Version: statusTestVersion, Managers: managers,
	})
	if ready {
		server.MarkReady()
	}

	done := make(chan error, 1)
	go func() { done <- server.ServeStatus() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, server.Close(ctx))
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("status Serve did not finish")
		}
	})
	transport := (&apiServer{socket: socket}).client(t, "")

	return &statusHarness{
		socket: socket, server: server, managers: managers,
		client: coagentv1connect.NewDaemonServiceClient(transport),
	}
}

func (h *statusHarness) status(t *testing.T) *coagentv1.GetStatusResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	status, err := h.client.GetStatus(ctx, &coagentv1.GetStatusRequest{})
	require.NoError(t, err)

	return status
}

func enabled() *bool {
	v := true

	return &v
}
