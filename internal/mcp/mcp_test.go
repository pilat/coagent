package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// svc.Stop (F7) moves client Close outside s.mu; this covers the base contract —
// every client is closed and the map cleared.
func TestService_StopClosesClientsAndClearsMap(t *testing.T) {
	closed := 0

	s := &svc{
		clients: map[string]*Client{
			"a": {name: "a", client: nopMCPClient{}, cancelRun: func() { closed++ }},
		},
	}

	s.Stop()

	s.mu.RLock()
	assert.Empty(t, s.clients)
	s.mu.RUnlock()

	assert.Equal(t, 1, closed, "Stop must close every client")
}
