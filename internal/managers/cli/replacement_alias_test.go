package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
)

func TestReplacementAliasAcceptsTerminalOldSessionDuringLifecycleDelivery(t *testing.T) {
	manager := &Manager{sessionID: 2, generation: 9, replacements: map[int64]int64{1: 2}}

	assert.NoError(t, manager.requireOwnedSession(1))
	assert.NoError(t, manager.requireOwnedSession(2))
	assert.Error(t, manager.requireOwnedSession(3))
}

func TestClosedSessionIsReopenedAsANewChat(t *testing.T) {
	manager := &Manager{sessionID: 0, closed: map[int64]struct{}{7: {}}}

	assert.True(t, manager.closedSession(7))
	assert.False(t, manager.closedSession(8))
}

// routingController resolves replacement chains server-side, the way the real
// bound controller does; the manager alias map is only a cache of its answers.
type routingController struct {
	*fakeController
	chain      map[int64]int64
	resolvedTo []int64
}

func (f *routingController) SendSessionMessageResolved(
	_ context.Context, d controllerapi.SessionMessageData,
) (int64, error) {
	accepted := d.SessionID
	for next, ok := f.chain[accepted]; ok; next, ok = f.chain[accepted] {
		accepted = next
	}

	f.resolvedTo = append(f.resolvedTo, accepted)

	return accepted, nil
}

// A send addressed at an old session whose replacement push has not been
// delivered yet must follow the durable chain instead of being rejected on a
// stale local alias cache.
func TestChatSendRoutesOldIDThroughTheDurableChainWhenTheAliasCacheIsCold(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cli")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "d.sock")
	srv, err := ctl.NewServer(context.Background(), socket, "test", ctl.Deps{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	fake := newFakeController()
	now := time.Now()
	fake.sessions = []controllerapi.SessionInfo{
		{ID: 1, ProjectID: chatProjectID, UpdatedAt: now.Add(-time.Hour)},
		{ID: 2, ProjectID: chatProjectID, UpdatedAt: now},
	}
	ctrl := &routingController{fakeController: fake, chain: map[int64]int64{1: 2}}

	mgr := New(ctrl, srv, "claude-sonnet-5", newFakeSecrets())
	require.NoError(t, mgr.Start(context.Background()))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	go func() { _ = srv.Serve(context.Background()) }()

	c, err := ctl.Dial(context.Background(), socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var open OpenResult
	require.NoError(t, c.Call(context.Background(), OpChatOpen, struct{}{}, &open))
	require.Equal(t, int64(2), open.SessionID)

	res := sendChat(t, c, SendParams{SessionID: 1, Text: "hello"})
	assert.Equal(t, int64(2), res.SessionID, "the old ID routes to its replacement")
	assert.Equal(t, []int64{2}, ctrl.resolvedTo)
}
