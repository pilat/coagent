package telegram

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

// The management root is the only session bound to the service topic: its
// messages ride ordinary session paths, its output renders in that same topic,
// and no lifecycle path may create, replace, or delete the topic itself.
func TestHarnessScenario_ServiceTopicMessageReachesManagementRoot(t *testing.T) {
	var calls []telegramHarnessCall

	controller := &fakeController{}
	controller.ensureRootID = 42
	manager := newTelegramHarnessManager(t, controller, &calls)
	manager.serviceTopicID = harnessTopicID
	manager.managementRootID = 42
	manager.registerTopic(42, harnessTopicID)

	manager.handleServiceTopicMessage(context.Background(), "plain operator question")

	require.Len(t, controller.messageCalls, 1, "ordinary text must reach the management root")
	assert.Equal(t, int64(42), controller.messageCalls[0].SessionID)
	assert.Equal(t, "plain operator question", controller.messageCalls[0].Message)
}

func TestHarnessScenario_ServiceTopicCommandsRideManagementRoot(t *testing.T) {
	var calls []telegramHarnessCall

	controller := &fakeController{}
	controller.ensureRootID = 42
	manager := newTelegramHarnessManager(t, controller, &calls)
	manager.serviceTopicID = harnessTopicID
	manager.managementRootID = 42
	manager.registerTopic(42, harnessTopicID)

	for _, command := range []string{"/config", "/clear", "/stop"} {
		manager.handleServiceTopicMessage(context.Background(), command)
	}

	require.Len(t, controller.messageCalls, 3, "session commands must be forwarded to the management root")
	for _, call := range controller.messageCalls {
		assert.Equal(t, int64(42), call.SessionID)
	}
}

func TestHarnessScenario_KillPickerExcludesManagementRoot(t *testing.T) {
	sessions := []controllerapi.SessionInfo{
		{ID: 42, Name: "management"},
		{ID: 7, Name: "worker"},
	}

	kept := filterOutManagementRoot(sessions, 42)
	require.Len(t, kept, 1)
	assert.Equal(t, int64(7), kept[0].ID)

	assert.Len(t, filterOutManagementRoot(sessions, 0), 2, "no root id keeps the picker unchanged")
}

func TestHarnessScenario_ManagementDeliveryAlwaysResolvesServiceTopic(t *testing.T) {
	var calls []telegramHarnessCall

	controller := &fakeController{}
	manager := newTelegramHarnessManager(t, controller, &calls)
	manager.serviceTopicID = harnessTopicID

	transport := &outputTransport{manager: manager}

	// A stale durable binding must not win: the transport patches it to the
	// manager's current service topic without creating a topic.
	topicID, patch, err := transport.ensureSessionTopic(context.Background(), &controllerapi.OutputClaimData{
		SessionID: 42,
		SessionAttributes: map[string]any{
			controllerapi.SessionAttributeManagementSurface: "service-topic",
			controllerapi.SessionAttributeTelegramTopicID:   int64(9999),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(harnessTopicID), topicID)
	assert.Equal(t, map[string]any{controllerapi.SessionAttributeTelegramTopicID: int64(harnessTopicID)}, patch)
}

func TestHarnessScenario_ManagementRootCloseSparesServiceTopic(t *testing.T) {
	var calls []telegramHarnessCall

	controller := &fakeController{}
	manager := newTelegramHarnessManager(t, controller, &calls)
	manager.serviceTopicID = harnessTopicID
	manager.registerTopic(42, harnessTopicID)

	transport := &outputTransport{manager: manager}
	result := transport.closeSession(context.Background(), &controllerapi.OutputClaimData{
		SessionID: 42,
		SessionAttributes: map[string]any{
			controllerapi.SessionAttributeManagementSurface: "service-topic",
		},
	})
	assert.Empty(t, result.Error)

	assert.NotContains(t, calls, "deleteForumTopic", "closing a management root must not delete the service topic")
}

// The /kill exclusion must survive /clear: the clear notification swaps the
// exclusion ID to the successor, so the picker keeps skipping the live root.
func TestHarnessScenario_ClearNotificationRefreshesManagementRootID(t *testing.T) {
	var calls []telegramHarnessCall

	controller := &fakeController{}
	manager := newTelegramHarnessManager(t, controller, &calls)
	manager.serviceTopicID = harnessTopicID
	manager.managementRootID = 42
	manager.registerTopic(42, harnessTopicID)

	manager.handleNotification(context.Background(), controllerapi.SessionNotification{
		SessionID: 42,
		Notification: sessionevent.Notification{
			Type:         sessionevent.NotifySessionCleared,
			OldSessionID: 42,
			NewSessionID: 77,
		},
	})

	assert.Equal(t, int64(77), manager.managementRootID)

	sessions := []controllerapi.SessionInfo{
		{ID: 77, Name: "management successor"},
		{ID: 7, Name: "worker"},
	}
	kept := filterOutManagementRoot(sessions, manager.managementRootID)
	require.Len(t, kept, 1)
	assert.Equal(t, int64(7), kept[0].ID)
}

// A forged kill:<management-root> callback never reaches the session.
func TestHarnessScenario_ForgedKillCallbackCannotReachManagementRoot(t *testing.T) {
	var calls []telegramHarnessCall

	controller := &fakeController{}
	manager := newTelegramHarnessManager(t, controller, &calls)
	manager.serviceTopicID = harnessTopicID
	manager.managementRootID = 42
	manager.cfg.AllowedUserIDs = []int64{7}
	manager.target = forumTarget{chatID: harnessChatID}

	manager.handleCallback(context.Background(), &telegramCallbackData{
		ID:   "cb-1",
		From: &telegramUser{ID: 7},
		Message: &telegramCallbackMeta{
			Chat:            telegramChat{ID: harnessChatID},
			MessageID:       1,
			MessageThreadID: harnessTopicID,
		},
		Data: "kill:42",
	})

	assert.Empty(t, controller.messageCalls, "the management root must not receive /kill")
}
