package daemon

import (
	"context"
	"errors"
	"sync"

	"github.com/pilat/coagent/internal/subagent"
)

// errLinkRead is the sentinel every ledger-failure test asserts on.
var errLinkRead = errors.New("link store unavailable")

// An unreadable ledger must not let spawning bypass the depth cap.
type failingLinkReads struct {
	subagent.Store
}

type blockingRecoveryLinks struct {
	subagent.Store

	entered     chan struct{}
	cancelled   chan struct{}
	allowReturn chan struct{}
	once        sync.Once
}

func (failingLinkReads) GetLink(context.Context, int64) (*subagent.Link, error) {
	return nil, errLinkRead
}

func (s *blockingRecoveryLinks) ListRunningChildLinks(ctx context.Context) ([]subagent.Link, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	close(s.cancelled)
	<-s.allowReturn
	return nil, ctx.Err()
}
