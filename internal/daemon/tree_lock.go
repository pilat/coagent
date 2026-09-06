package daemon

import (
	"context"
	"fmt"
)

type sessionTreeLock struct {
	token chan struct{}
}

func newSessionTreeLock() *sessionTreeLock {
	lock := &sessionTreeLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}

	return lock
}

func (l *sessionTreeLock) acquire(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("acquire session tree lock: %w", ctx.Err())
	case <-l.token:
		return func() { l.token <- struct{}{} }, nil
	}
}

//nolint:wsl_v5 // Resolution and acquisition must remain one keyed-lock operation.
func (s *svc) lockSessionTree(ctx context.Context, sessionID int64) (func(), error) {
	store := s.treeStore
	if store == nil {
		store = s.sessionStore
	}

	record, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session tree lock: %w", err)
	}

	rootID := sessionRootID(record)
	value, _ := s.treeLocks.LoadOrStore(rootID, newSessionTreeLock())
	lock, ok := value.(*sessionTreeLock)
	if !ok {
		return nil, fmt.Errorf("invalid session tree lock for root %d", rootID)
	}

	return lock.acquire(ctx)
}
