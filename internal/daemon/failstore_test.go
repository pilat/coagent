package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type childStateSessionStore struct {
	Store
	record *sessionstore.SessionRecord
	reads  int
}

func (s *childStateSessionStore) GetSession(context.Context, int64) (*sessionstore.SessionRecord, error) {
	s.reads++

	return s.record, nil
}

type panickingChildCommitStore struct {
	*sessionstore.Store
	once sync.Once
}

func (s *panickingChildCommitStore) Commit(
	ctx context.Context,
	commit sessionstore.Commit,
) (*sessionstore.CommitResult, error) {
	if commit.ObserveBudget && commit.State.Iteration != nil && *commit.State.Iteration > 0 {
		record, err := s.GetSession(ctx, commit.SessionID)
		if err != nil {
			return nil, fmt.Errorf("load panic fixture session: %w", err)
		}

		if record.ParentID != 0 {
			s.once.Do(func() { panic("boom in child") })
		}
	}

	result, err := s.Store.Commit(ctx, commit)
	if err != nil {
		return nil, fmt.Errorf("commit panic fixture session: %w", err)
	}

	return result, nil
}

type deliveringChildLinks struct {
	subagent.Store
	link      subagent.Link
	delivered bool
}

func (s *deliveringChildLinks) ListPendingChildLinks(ctx context.Context, parentID int64) ([]subagent.Link, error) {
	if parentID == s.link.ParentID && !s.delivered {
		won, err := s.DeliverCompletion(ctx, s.link, "child finished during ownership capture")
		if err != nil {
			return nil, fmt.Errorf("deliver ownership handoff: %w", err)
		}

		if !won {
			return nil, fmt.Errorf("ownership handoff did not commit for child %d", s.link.ChildID)
		}

		s.delivered = true
	}

	links, err := s.Store.ListPendingChildLinks(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("read handed-off child links: %w", err)
	}

	return links, nil
}

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

// failGetLink arms GetLink to fail from call number `from` onwards, optionally
// only for childID.
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

type failFirstRemoveScheduleStore struct {
	schedule.Store
	once      sync.Once
	attempted chan struct{}
}

func (s *failFirstRemoveScheduleStore) RemoveSchedule(ctx context.Context, id int64) error {
	failed := false
	s.once.Do(func() {
		failed = true
		close(s.attempted)
	})
	if failed {
		return assert.AnError
	}

	return s.Store.RemoveSchedule(ctx, id)
}

// newTestLinkStore opens a migrated temp SQLite DB and returns a *sessionstore.Store
// (for the session/message rows link tests reference), a subagent.Store, and a project
// id the sessions can reference (FKs are enforced).
func newTestLinkStore(t *testing.T) (*sessionstore.Store, subagent.Store, int64) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	res, err := db.ExecContext(
		context.Background(),
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		t.TempDir(), "test",
	)
	require.NoError(t, err)
	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	sessions := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessions)
	return sessions, links, projectID
}

type blockingCreateSessionStore struct {
	Store
	entered, release chan struct{}
}

func (s *blockingCreateSessionStore) CreateReplacementSession(
	ctx context.Context,
	oldID int64,
) (*sessionstore.SessionRecord, error) {
	close(s.entered)
	<-s.release
	return s.Store.CreateReplacementSession(ctx, oldID)
}

// countingSessionStore decorates a real session store so the publish gate's
// lookups can be counted and made to fail on demand.
type countingSessionStore struct {
	Store

	mu       sync.Mutex
	getCalls int
	failNth  int // fail exactly the Nth GetSession call; 0 = never
}

type staleReadSessionStore struct {
	Store
	target      int64
	read        chan struct{}
	release     chan struct{}
	mu          sync.Mutex
	intercepted bool
}

func (c *countingSessionStore) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	c.mu.Lock()
	c.getCalls++
	n, fail := c.getCalls, c.failNth
	c.mu.Unlock()

	if fail > 0 && n == fail {
		return nil, errSessionRead
	}

	return c.Store.GetSession(ctx, id)
}

func (c *countingSessionStore) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.getCalls
}

func (s *staleReadSessionStore) GetSession(
	ctx context.Context,
	id int64,
) (*sessionstore.SessionRecord, error) {
	record, err := s.Store.GetSession(ctx, id)
	if err != nil || id != s.target {
		return record, err
	}

	s.mu.Lock()
	intercept := !s.intercepted
	s.intercepted = true
	s.mu.Unlock()

	if intercept {
		close(s.read)
		<-s.release
	}

	return record, nil
}

type blockingRecoveryLinks struct {
	subagent.Store

	entered     chan struct{}
	cancelled   chan struct{}
	allowReturn chan struct{}
	once        sync.Once
}

type blockingProgressStop struct {
	progressruntime.Service

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingProgressStop) Stop(context.Context) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release

	return nil
}

func (s *blockingRecoveryLinks) ListRunningChildLinks(ctx context.Context) ([]subagent.Link, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	close(s.cancelled)
	<-s.allowReturn

	return nil, ctx.Err()
}

type blockingStartupProcesses struct {
	backgroundprocess.Service
	entered chan struct{}
	release chan struct{}
}

func (s *blockingStartupProcesses) InterruptNonterminal(ctx context.Context) (int, error) {
	close(s.entered)
	<-s.release
	return s.Service.InterruptNonterminal(ctx)
}

type stoppingGateStore struct {
	Store
	sessionID   int64
	written     chan struct{}
	release     chan struct{}
	writtenOnce sync.Once
	releaseOnce sync.Once
}

func (s *stoppingGateStore) UpdateSessionStatus(
	ctx context.Context,
	id int64,
	status sessionstore.SessionStatus,
) error {
	if err := s.Store.UpdateSessionStatus(ctx, id, status); err != nil {
		return err
	}

	if id == s.sessionID && status == sessionstore.SessionStatusStopping {
		s.writtenOnce.Do(func() { close(s.written) })
		<-s.release
	}

	return nil
}

// failingGetSessionStore makes exactly one GetSession call fail, simulating a
// transient store hiccup while everything else delegates to the real store.
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

func newTestStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	s, _ := newTestStoreWithSchedule(t)
	return s
}

func newTestStoreWithSchedule(t *testing.T) (*sessionstore.Store, schedule.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))
	return sessionstore.NewStore(db), schedule.NewStore(db, sessionstore.NewStore(db))
}
