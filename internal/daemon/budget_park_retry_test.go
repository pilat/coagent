package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/sessionstore"
)

type blockingBudgetParkService struct {
	budget.Service
	entered chan struct{}
	release chan struct{}
}

func (s blockingBudgetParkService) BeginDrain(
	context.Context, int64, int64, string,
) (*sessionstore.BudgetRecord, error) {
	s.entered <- struct{}{}
	<-s.release

	return nil, errBudgetParkProbe
}

func TestStartBudgetParkDeduplicatesActiveAttemptAndAllowsRetry(t *testing.T) {
	service := blockingBudgetParkService{
		entered: make(chan struct{}, 2), release: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager := &svc{budgetCtx: ctx, budgetSvc: service}
	record := &sessionstore.BudgetRecord{
		RootSessionID: 1, State: sessionstore.BudgetFired,
		Generation: 2, ParkPhase: budgetParkRequested, ParkOwner: "owner",
	}

	manager.startBudgetPark(record)
	requireBudgetParkSignal(t, service.entered)
	manager.startBudgetPark(record)
	select {
	case <-service.entered:
		t.Fatal("duplicate park attempt ran while the first was active")
	default:
	}

	close(service.release)
	joinBudgetParkWorkers(t, manager)
	manager.startBudgetPark(record)
	requireBudgetParkSignal(t, service.entered)
	joinBudgetParkWorkers(t, manager)
	assert.Empty(t, manager.budgetParks)
}

func requireBudgetParkSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("budget park worker did not start")
	}
}

func joinBudgetParkWorkers(t *testing.T, manager *svc) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		manager.budgetWG.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("budget park worker did not stop")
	}
}
