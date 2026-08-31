package daemon

import (
	"context"
	"errors"
	"sync"

	"github.com/pilat/coagent/internal/subagent"
)

// errLinkRead is the sentinel every ledger-failure test asserts on.
var errLinkRead = errors.New("link store unavailable")

// flakyLinkStore decorates a real subagent.Store so individual ledger operations can
// be made to fail on demand. Everything not overridden delegates to the embedded
// store, so a live daemon keeps working around the injected failure.
type flakyLinkStore struct {
	subagent.Store

	mu sync.Mutex

	// getLinkFailFrom > 0 makes GetLink fail from its Nth call onwards (1 =
	// always). getLinkFailFor restricts that to one child id (0 = every id).
	getLinkFailFrom int
	getLinkFailFor  int64
	getLinkCalls    map[int64]int

	// getLinkFailOnly fails exactly the Nth call, modelling an intermittent read.
	getLinkFailOnly int

	// markTerminalFailN fails the first N MarkLinkTerminal calls; -1 = always.
	markTerminalFailN int
	markTerminalCalls int

	listPendingFail bool
	listRunningFail bool
}

func newFlakyLinkStore(inner subagent.Store) *flakyLinkStore {
	return &flakyLinkStore{Store: inner, getLinkCalls: make(map[int64]int)}
}

func (f *flakyLinkStore) GetLink(ctx context.Context, childID int64) (*subagent.Link, error) {
	f.mu.Lock()
	f.getLinkCalls[childID]++
	n := f.getLinkCalls[childID]
	from, forID, onlyNth := f.getLinkFailFrom, f.getLinkFailFor, f.getLinkFailOnly
	f.mu.Unlock()

	scoped := forID == 0 || forID == childID

	if from > 0 && n >= from && scoped {
		return nil, errLinkRead
	}

	if onlyNth > 0 && n == onlyNth && scoped {
		return nil, errLinkRead
	}

	return f.Store.GetLink(ctx, childID)
}

func (f *flakyLinkStore) MarkLinkTerminal(
	ctx context.Context,
	childID int64,
	state subagent.State,
	result string,
	outcome subagent.Outcome,
) error {
	f.mu.Lock()
	f.markTerminalCalls++
	n, limit := f.markTerminalCalls, f.markTerminalFailN
	f.mu.Unlock()

	if limit < 0 || n <= limit {
		return errLinkRead
	}

	return f.Store.MarkLinkTerminal(ctx, childID, state, result, outcome)
}

func (f *flakyLinkStore) ListPendingChildLinks(ctx context.Context, parentID int64) ([]subagent.Link, error) {
	if f.listPendingFail {
		return nil, errLinkRead
	}

	return f.Store.ListPendingChildLinks(ctx, parentID)
}

func (f *flakyLinkStore) ListRunningChildLinks(ctx context.Context) ([]subagent.Link, error) {
	if f.listRunningFail {
		return nil, errLinkRead
	}

	return f.Store.ListRunningChildLinks(ctx)
}

// markTerminalAttempts reports how many times MarkLinkTerminal was called.
func (f *flakyLinkStore) markTerminalAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.markTerminalCalls
}

// failGetLink arms GetLink to fail from call number `from` onwards, optionally
// only for childID.
func (f *flakyLinkStore) failGetLink(from int, childID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.getLinkFailFrom = from
	f.getLinkFailFor = childID
}

type flakyActivationStore struct {
	subagent.Transactions

	mu    sync.Mutex
	failN int
	calls int
}

func (f *flakyActivationStore) TryFinalizeActivation(
	ctx context.Context,
	childID int64,
	state subagent.State,
	result string,
	outcome subagent.Outcome,
) (bool, error) {
	f.mu.Lock()
	f.calls++
	n, limit := f.calls, f.failN
	f.mu.Unlock()

	if limit < 0 || n <= limit {
		return false, errLinkRead
	}

	return f.Transactions.TryFinalizeActivation(ctx, childID, state, result, outcome)
}

func (f *flakyActivationStore) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}
