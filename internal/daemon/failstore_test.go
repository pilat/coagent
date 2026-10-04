package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// A live parent-child ledger exposes retries when link operations fail on demand.
type ledgerHarness struct {
	*harness

	flaky      *flakyLinkStore
	activation *flakyActivationStore
	parentID   int64
	childID    int64
}

func newLedgerHarness(t *testing.T) *ledgerHarness {
	t.Helper()
	var flaky *flakyLinkStore
	h := newHarness(t, harnessOptions{respond: trivialRespond, links: func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	}})
	activation := &flakyActivationStore{Store: h.mgr.links}
	h.mgr.links = activation
	parentID := h.createRoot(nil)
	childID := h.createChild(parentID, subagent.Link{TaskCallID: "bg"})
	return &ledgerHarness{harness: h, flaky: flaky, activation: activation, parentID: parentID, childID: childID}
}

// errLinkRead is the sentinel every ledger-failure test asserts on.
var errLinkRead = errors.New("link store unavailable")

// Unmodified operations delegate to the real ledger, keeping the live daemon intact around an injected failure.
type flakyLinkStore struct {
	subagent.Store

	mu sync.Mutex

	// The failure threshold selects calls; an optional child ID narrows which link reads fail.
	getLinkFailFrom int
	getLinkFailFor  int64
	getLinkCalls    map[int64]int

	// getLinkFailOnly fails exactly the Nth call, modelling an intermittent read.
	getLinkFailOnly int

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

// failGetLink arms GetLink to fail from call number `from` onwards, optionally only for childID.
func (f *flakyLinkStore) failGetLink(from int, childID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getLinkFailFrom = from
	f.getLinkFailFor = childID
}

type flakyActivationStore struct {
	subagent.Store

	mu       sync.Mutex
	failN    int
	calls    int
	failRead bool
}

func (f *flakyActivationStore) Finalize(ctx context.Context, childID int64, errored bool) (*subagent.Link, error) {
	f.mu.Lock()
	f.calls++
	n, limit, read := f.calls, f.failN, f.failRead
	f.mu.Unlock()
	if read {
		return nil, errLinkRead
	}
	if limit < 0 || n <= limit {
		link, _ := f.GetLink(ctx, childID)
		return link, errLinkRead
	}
	return f.Store.Finalize(ctx, childID, errored)
}

func (f *flakyActivationStore) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type blockingRecoveryLinks struct {
	subagent.Store

	entered     chan struct{}
	cancelled   chan struct{}
	allowReturn chan struct{}
	once        sync.Once
}

func (s *blockingRecoveryLinks) ListRunningChildLinks(ctx context.Context) ([]subagent.Link, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	close(s.cancelled)
	<-s.allowReturn
	return nil, ctx.Err()
}

// Fail exactly one session read to expose transient recovery without replacing the real store.
type failingGetSessionStore struct {
	Store
	err     error
	pending atomic.Bool
	calls   atomic.Int32
	skip    int32
}

func (s *failingGetSessionStore) GetSession(
	ctx context.Context,
	id int64,
) (*sessionstore.SessionRecord, error) {
	if s.calls.Add(1) <= s.skip {
		return s.Store.GetSession(ctx, id)
	}
	if s.pending.CompareAndSwap(true, false) {
		return nil, s.err
	}
	return s.Store.GetSession(ctx, id)
}
