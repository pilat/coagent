package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/sessionlifecycle"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

var errBudgetParkProbe = errors.New("budget park probe")

type budgetServiceProbe struct {
	budget.Service
	record       *sessionstore.BudgetRecord
	getErr       error
	releaseErr   error
	releaseCalls int
	beginCalls   chan struct{}
}

func (s *budgetServiceProbe) Get(context.Context, int64) (*sessionstore.BudgetRecord, error) {
	return s.record, s.getErr
}

func (s *budgetServiceProbe) Release(
	context.Context,
	int64,
	int64,
	string,
) (*sessionstore.BudgetRecord, error) {
	s.releaseCalls++

	return s.record, s.releaseErr
}

func (s *budgetServiceProbe) BeginDrain(
	context.Context,
	int64,
	int64,
	string,
) (*sessionstore.BudgetRecord, error) {
	s.beginCalls <- struct{}{}

	return nil, errBudgetParkProbe
}

type budgetGateStoreProbe struct {
	sessionstore.AgentRuntimeStore
	response   *sessionstore.BudgetedResponseResult
	compaction *sessionstore.BudgetedCompactionResult
}

func (s budgetGateStoreProbe) InsertBudgetedResponse(
	context.Context,
	sessionstore.BudgetedResponse,
) (*sessionstore.BudgetedResponseResult, error) {
	return s.response, nil
}

func (s budgetGateStoreProbe) ReplaceCompactedMessagesBudgeted(
	context.Context,
	sessionstore.BudgetedCompaction,
) (*sessionstore.BudgetedCompactionResult, error) {
	return s.compaction, nil
}

type budgetParkSessionStore struct {
	sessionstore.OrchestrationStore
}

func (budgetParkSessionStore) ListAllSessions(context.Context) ([]*sessionstore.SessionRecord, error) {
	return nil, nil
}

type budgetStopperProbe struct {
	sessionlifecycle.Stopper
	beginCalls chan struct{}
}

func (s budgetStopperProbe) Begin(context.Context, int64) (*sessionlifecycle.StopPlan, error) {
	s.beginCalls <- struct{}{}

	return nil, errBudgetParkProbe
}

func newSessionBudgetGateProbe(
	t *testing.T,
	fired bool,
) (*sessionBudgetGate, *budgetServiceProbe) {
	t.Helper()

	record := &sessionstore.BudgetRecord{
		RootSessionID: 1,
		State:         sessionstore.BudgetFired,
		Generation:    2,
		ParkPhase:     budgetParkRequested,
		ParkOwner:     "owner",
	}
	service := &budgetServiceProbe{beginCalls: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	manager := &svc{budgetCtx: ctx, budgetCancel: cancel, budgetSvc: service}
	t.Cleanup(func() {
		cancel()
		manager.budgetWG.Wait()
	})

	return &sessionBudgetGate{
		daemon: manager,
		store: budgetGateStoreProbe{
			response: &sessionstore.BudgetedResponseResult{Fired: fired, Budget: record},
			compaction: &sessionstore.BudgetedCompactionResult{
				Fired: fired, Budget: record,
			},
		},
		sessionID: 1,
		rootID:    1,
	}, service
}

func TestSessionBudgetGateStartsRequestedPark(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(*sessionBudgetGate) error
	}{
		{
			name: "response",
			run: func(gate *sessionBudgetGate) error {
				_, _, _, err := gate.PersistResponse(t.Context(), &transcript.Message{Role: "assistant"}, "")

				return err
			},
		},
		{
			name: "compaction",
			run: func(gate *sessionBudgetGate) error {
				_, _, err := gate.PersistCompaction(t.Context(), sessionstore.BudgetedCompaction{})

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate, service := newSessionBudgetGateProbe(t, true)

			require.NoError(t, tt.run(gate))
			select {
			case <-service.beginCalls:
			case <-time.After(time.Second):
				t.Fatal("requested budget park did not begin")
			}
		})
	}
}

func TestSessionBudgetGateDoesNotStartUnfiredPark(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(*sessionBudgetGate) error
	}{
		{
			name: "response",
			run: func(gate *sessionBudgetGate) error {
				_, _, _, err := gate.PersistResponse(t.Context(), &transcript.Message{Role: "assistant"}, "")

				return err
			},
		},
		{
			name: "compaction",
			run: func(gate *sessionBudgetGate) error {
				_, _, err := gate.PersistCompaction(t.Context(), sessionstore.BudgetedCompaction{})

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate, service := newSessionBudgetGateProbe(t, false)

			require.NoError(t, tt.run(gate))
			select {
			case <-service.beginCalls:
				t.Fatal("unfired budget unexpectedly began parking")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestSessionBudgetGateOnlyStartsRequestedPhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(*sessionBudgetGate) error
	}{
		{
			name: "response",
			run: func(gate *sessionBudgetGate) error {
				_, _, _, err := gate.PersistResponse(t.Context(), &transcript.Message{Role: "assistant"}, "")

				return err
			},
		},
		{
			name: "compaction",
			run: func(gate *sessionBudgetGate) error {
				_, _, err := gate.PersistCompaction(t.Context(), sessionstore.BudgetedCompaction{})

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := &sessionstore.BudgetRecord{
				RootSessionID: 1,
				State:         sessionstore.BudgetFired,
				Generation:    2,
				ParkPhase:     "draining",
				ParkOwner:     "owner",
			}
			service := &budgetServiceProbe{beginCalls: make(chan struct{}, 1)}
			stopper := budgetStopperProbe{beginCalls: make(chan struct{}, 1)}
			ctx, cancel := context.WithCancel(t.Context())
			manager := &svc{
				budgetCtx: ctx, budgetCancel: cancel, budgetSvc: service,
				sessionStore: budgetParkSessionStore{}, stopper: stopper,
			}
			t.Cleanup(func() {
				cancel()
				manager.budgetWG.Wait()
			})
			gate := &sessionBudgetGate{
				daemon: manager,
				store: budgetGateStoreProbe{
					response: &sessionstore.BudgetedResponseResult{Fired: true, Budget: record},
					compaction: &sessionstore.BudgetedCompactionResult{
						Fired: true, Budget: record,
					},
				},
				sessionID: 1,
				rootID:    1,
			}

			require.NoError(t, tt.run(gate))
			select {
			case <-stopper.beginCalls:
				t.Fatal("non-requested budget phase unexpectedly began parking")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestRegisterBudgetToolOnlyForRootWithService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		service budget.Service
		parent  int64
		want    bool
	}{
		{name: "no service"},
		{name: "child", service: &budgetServiceProbe{}, parent: 7},
		{name: "root", service: &budgetServiceProbe{}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &mockSession{}
			manager := &svc{budgetSvc: tt.service}

			manager.registerBudgetTool(t.Context(), &sessionstore.SessionRecord{
				ID: 11, ParentID: tt.parent,
			}, sess)

			assert.Equal(t, tt.want, sess.hasTool(budget.ToolID))
		})
	}
}

func TestModelHasPricingRequiresMatchingPricedEntry(t *testing.T) {
	t.Parallel()

	manager := &svc{modelEntries: []config.ModelEntry{
		{ID: "unpriced"},
		{ID: "priced", Pricing: &config.ModelPricing{}},
	}}

	assert.False(t, manager.modelHasPricing("missing"))
	assert.False(t, manager.modelHasPricing("unpriced"))
	assert.True(t, manager.modelHasPricing("priced"))
}

func TestReleaseArmedBudgetHonorsStateAndErrors(t *testing.T) {
	t.Parallel()

	t.Run("released budget is unchanged", func(t *testing.T) {
		service := &budgetServiceProbe{record: &sessionstore.BudgetRecord{State: sessionstore.BudgetReleased}}
		manager := &svc{budgetSvc: service}

		require.NoError(t, manager.releaseArmedBudget(t.Context(), 1, "stopped"))
		assert.Zero(t, service.releaseCalls)
	})

	t.Run("armed budget is released", func(t *testing.T) {
		service := &budgetServiceProbe{record: &sessionstore.BudgetRecord{
			State: sessionstore.BudgetArmed, Generation: 3,
		}}
		manager := &svc{budgetSvc: service}

		require.NoError(t, manager.releaseArmedBudget(t.Context(), 1, "stopped"))
		assert.Equal(t, 1, service.releaseCalls)
	})

	t.Run("release error is preserved", func(t *testing.T) {
		releaseErr := errors.New("release failed")
		service := &budgetServiceProbe{
			record:     &sessionstore.BudgetRecord{State: sessionstore.BudgetArmed},
			releaseErr: releaseErr,
		}
		manager := &svc{budgetSvc: service}

		err := manager.releaseArmedBudget(t.Context(), 1, "stopped")
		require.ErrorIs(t, err, releaseErr)
		assert.Equal(t, 1, service.releaseCalls)
	})
}
