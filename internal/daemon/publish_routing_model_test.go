package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

// TestPublishRoutingModel compares the real durable-route adapter with the
// reference rule: a root event is delivered to exactly its one manager owner,
// while an ownerless root is delivered to none. The trace includes a warm-cache
// claim and clear/recreation because those are the transitions most likely to
// accidentally retain or lose an owner.
func TestPublishRoutingModel_ManagerOwnershipSurvivesTransitions(t *testing.T) {
	t.Parallel()

	mgr, _, store := newTestManager(t)
	ctx := context.Background()
	subscribers := map[string]<-chan controllerapi.SessionNotification{
		"alpha": mgr.PubSub().SubscribeManager("alpha"),
		"beta":  mgr.PubSub().SubscribeManager("beta"),
	}
	model := make(map[int64]string)
	pid := testProject(t, store, "/tmp/publish-routing-model")

	create := func(owner string) int64 {
		t.Helper()
		attributes := map[string]any(nil)
		if owner != "" {
			attributes = map[string]any{controllerapi.SessionAttributeManagerID: owner}
		}
		record, err := mgr.sessionStore.CreateSession(ctx, pid, "fake-model", "", attributes)
		require.NoError(t, err)
		model[record.ID] = owner

		return record.ID
	}
	publish := func(sessionID int64, message string) {
		t.Helper()
		mgr.NotifySession(sessionID, sessionevent.Notification{
			Type: sessionevent.NotifyMessage, Message: message,
		})
		assertModelDeliveries(t, subscribers, model[sessionID], sessionID, message)
	}

	alphaID := create("alpha")
	betaID := create("beta")
	claimedID := create("")
	publish(alphaID, "alpha owns this")
	publish(betaID, "beta owns this")
	publish(claimedID, "nobody owns this")

	require.NoError(t, mgr.SetAttributes(ctx, claimedID, map[string]any{
		controllerapi.SessionAttributeManagerID: "alpha",
	}))
	model[claimedID] = "alpha"
	publish(claimedID, "alpha claimed the warm route")

	// A restarted daemon begins with empty route caches and must recover the
	// exact owner from the durable session record.
	mgr.childMu.Lock()
	mgr.childCache = make(map[int64]bool)
	mgr.ownerCache = make(map[int64]string)
	mgr.childMu.Unlock()
	publish(alphaID, "alpha survives a cold route cache")

	newAlphaID, err := mgr.Clear(ctx, alphaID)
	require.NoError(t, err)
	model[newAlphaID] = model[alphaID]
	assertModelDeliveries(t, subscribers, "alpha", alphaID, "")
	publish(newAlphaID, "clear preserved alpha")
}

func assertModelDeliveries(
	t *testing.T,
	subscribers map[string]<-chan controllerapi.SessionNotification,
	wantOwner string,
	wantSessionID int64,
	wantMessage string,
) {
	t.Helper()

	var gotOwners []string
	for managerID, channel := range subscribers {
		select {
		case notification := <-channel:
			gotOwners = append(gotOwners, managerID)
			assert.Equal(t, wantSessionID, notification.SessionID)
			if wantMessage != "" {
				assert.Equal(t, wantMessage, notification.Notification.Message)
			}
		default:
		}
	}
	slices.Sort(gotOwners)

	wantOwners := []string(nil)
	if wantOwner != "" {
		wantOwners = []string{wantOwner}
	}
	assert.Equal(t, wantOwners, gotOwners)
}
