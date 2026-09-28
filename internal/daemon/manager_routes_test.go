package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestManagerRoutesReplacementAllowsRetirementPublication(t *testing.T) {
	for _, failRetirement := range []bool{false, true} {
		name := "retired"
		if failRetirement {
			name = "retirement failed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mgr, _, projects := newTestManager(t)
			projectID := testProject(t, projects, t.TempDir())
			record, err := mgr.sessionStore.CreateSession(t.Context(), projectID, "model", "", map[string]any{
				controllerapi.SessionAttributeManagerID: "alpha",
			})
			require.NoError(t, err)
			routes := newManagerRoutes(mgr.sessionStore, mgr.managerRoots, projects, sessionbus.New())
			events := routes.Source().SubscribeManager("alpha")
			result := make(chan struct {
				id  int64
				err error
			}, 1)
			go func() {
				id, replaceErr := routes.Replace(
					t.Context(),
					record.ID,
					0,
					func(ctx context.Context, oldID int64) error {
						routes.Publish(
							oldID,
							sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "retiring"},
						)
						if failRetirement {
							return errors.New("retirement failed")
						}
						return mgr.sessionStore.MarkSessionKilled(ctx, oldID)
					},
				)
				result <- struct {
					id  int64
					err error
				}{id: id, err: replaceErr}
			}()

			var replacementID int64
			select {
			case completed := <-result:
				require.NoError(t, completed.err, "retirement failure must not discard the committed replacement")
				replacementID = completed.id
			case <-time.After(3 * time.Second):
				t.Fatal("retirement publication blocked replacement")
			}
			cleared := requireManagerNotification(t, events)
			assert.Equal(t, sessionevent.NotifySessionCleared, cleared.Notification.Type)
			assert.Equal(t, replacementID, cleared.Notification.NewSessionID)
			assert.Equal(t, "retiring", requireManagerNotification(t, events).Notification.Message)
			replacement, err := mgr.sessionStore.GetSession(t.Context(), replacementID)
			require.NoError(t, err)
			assert.Equal(t, "alpha", replacement.Attributes[controllerapi.SessionAttributeManagerID])
			old, err := mgr.sessionStore.GetSession(t.Context(), record.ID)
			require.NoError(t, err)
			if failRetirement {
				assert.Equal(t, sessionstore.SessionStatusTerminating, old.Status)
			} else {
				assert.NotNil(t, old.KilledAt)
			}
		})
	}
}
