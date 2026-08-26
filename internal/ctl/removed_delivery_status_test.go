package ctl

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestStatus_ReportsRemovedManagerBacklog(t *testing.T) {
	h := newHarness(t, &config.Config{UnifiedConfig: &config.UnifiedConfig{}})
	h.delivery.owners = []string{"gone"}
	h.delivery.values = map[string]controllerapi.OutputQueueStatusData{
		"gone": {Pending: 1, BlockedID: 42, BlockedForSec: 12, DeliveryError: "invalid target"},
	}

	status, err := h.dial(t).Status(context.Background())
	require.NoError(t, err)
	require.Len(t, status.Managers, 1)
	assert.Equal(t, ManagerStatus{
		ID: "gone", Driver: "removed", PendingOutputs: 1,
		BlockedOutputID: 42, BlockedForSeconds: 12, DeliveryError: "invalid target",
	}, status.Managers[0])
}
