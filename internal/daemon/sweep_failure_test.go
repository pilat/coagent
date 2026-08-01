package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
)

// TestSweep_PartialFailureIsNotSuccess: a failed pass used to end in a cheerful
// `sweep_done resumed=0` — crash recovery reporting success it never performed.
func TestSweep_PartialFailureIsNotSuccess(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	h.flaky.listRunningFail = true

	// Observe at Info: sweep_done is an Info line, so a Warn-level observer would
	// make both assertions vacuously pass.
	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.sweep(ctx)

	entries := logs.FilterMessage("sweep_incomplete").All()
	require.Len(t, entries, 1)

	fields := entries[0].ContextMap()
	assert.Equal(t, true, fields["running_failed"])
	assert.Equal(t, false, fields["undelivered_failed"], "PASS 2 still ran")

	assert.Empty(t, logs.FilterMessage("sweep_done").All(), "a partial sweep never reports done")
}

// TestSweep_CleanRunReportsDone: the healthy path keeps its existing line.
func TestSweep_CleanRunReportsDone(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.sweep(ctx)

	assert.Len(t, logs.FilterMessage("sweep_done").All(), 1)
	assert.Empty(t, logs.FilterMessage("sweep_incomplete").All())
}
