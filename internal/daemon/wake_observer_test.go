package daemon

import (
	"context"
	"sync"

	"github.com/pilat/coagent/internal/sessionstore"
)

type wakeObserver struct {
	Store
	sessionID int64
	observed  chan struct{}
	once      sync.Once
}

func (s *wakeObserver) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	record, err := s.Store.GetSession(ctx, id)
	if id == s.sessionID {
		s.once.Do(func() { close(s.observed) })
	}
	return record, err
}
