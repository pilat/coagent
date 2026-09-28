package daemon

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/inputruntime"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

type forwardingFactory struct {
	delegate     session.Factory
	processBound atomic.Bool
}

type observedResources struct {
	tool.ResourceLifecycle
	retired atomic.Int64
	closed  atomic.Bool
}

func TestNewRejectsMissingResourceOwner(t *testing.T) {
	service, err := New(
		t.Context(), nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil, nil,
	)
	require.ErrorContains(t, err, "daemon requires tool resources")
	require.Nil(t, service)
}

func (f *forwardingFactory) Create(ctx context.Context, opts session.CreateOptions) (session.Service, error) {
	f.processBound.Store(opts.ProcessService != nil)
	return f.delegate.Create(ctx, opts)
}

func (r *observedResources) Retire(id int64) error {
	r.retired.Store(id)
	return r.ResourceLifecycle.Retire(id)
}

func (r *observedResources) Close() error {
	r.closed.Store(true)
	return r.ResourceLifecycle.Close()
}

func mustNewSvc(
	ctx context.Context,
	t *testing.T,
	factory session.Factory,
	processStore backgroundprocess.Store,
	toolResources tool.ResourceLifecycle,
	store Store,
	sessionStore sessionstore.OrchestrationStore,
	inboxStore inputruntime.Store,
	runtimeStore sessionstore.AgentRuntimeStore,
	managerOutputs sessionstore.ManagerOutputStore,
	managerRoots sessionstore.ManagerRootTransactions,
	lifecycleStore sessionstore.SessionLifecycleStore,
	modelInputs sessionstore.ModelInputStore,
	links subagent.Store,
	subagents subagent.Transactions,
	budgetSvc budget.Service,
	progressStore progressruntime.Store,
	scheduleSvc schedule.Service,
	defaultModelFn func() string,
	applier configapply.Service,
) *svc {
	t.Helper()

	service, err := newSvc(
		ctx, factory, processStore, toolResources, store, sessionStore, inboxStore, runtimeStore,
		managerOutputs, managerRoots, lifecycleStore, modelInputs, links, subagents,
		budgetSvc, progressStore, scheduleSvc, defaultModelFn, applier,
	)
	require.NoError(t, err)

	return service
}

func mustNewTransactions(
	t *testing.T,
	db *sql.DB,
	invalidateCompletionCheck func(context.Context, *sql.Tx, int64, time.Time) error,
) subagent.Transactions {
	t.Helper()

	transactions, err := subagent.NewTransactions(db, invalidateCompletionCheck)
	require.NoError(t, err)

	return transactions
}
