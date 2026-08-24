package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

func TestHarnessModel_ServiceTopicBindingSurvivesRestart(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()
	require.NoError(t, os.Mkdir(filepath.Join(home, coagenthome.DirName), 0o700))

	remote := &serviceTopicHarness{nextID: 7001}
	first := remote.manager()
	bound, err := first.ensureServiceTopic(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(7001), bound)
	assert.Equal(t, 1, remote.created)

	second := remote.manager()
	restarted, err := second.ensureServiceTopic(context.Background())
	require.NoError(t, err)
	assert.Equal(t, bound, restarted)
	assert.Equal(t, 1, remote.created, "restart must reuse the durable binding")
	assert.Equal(t, []forumTopicUpdate{{
		ChatID: -100123, ThreadID: bound,
	}}, remote.topicUpdates)
}

func FuzzTelegramServiceTopicProtocol(f *testing.F) {
	f.Add([]byte{0, 1, 0})
	f.Add([]byte{1, 1, 1})
	f.Fuzz(func(t *testing.T, commands []byte) {
		if len(commands) > 32 {
			commands = commands[:32]
		}
		home := t.TempDir()
		restore := coagenthome.Override(home)
		defer restore()
		require.NoError(t, os.Mkdir(filepath.Join(home, coagenthome.DirName), 0o700))

		remote := &serviceTopicHarness{nextID: 7001}
		bound := false
		expectedCreates := 0
		for _, command := range commands {
			if command&1 != 0 {
				remote.missing = true
				bound = false
			}
			if !bound {
				expectedCreates++
				bound = true
			}
			_, err := remote.manager().ensureServiceTopic(context.Background())
			require.NoError(t, err)
		}
		assert.Equal(t, expectedCreates, remote.created, "only initial bind and proven deletions create topics")
	})
}

type serviceTopicHarness struct {
	nextID       int64
	created      int
	missing      bool
	topicUpdates []forumTopicUpdate
}

func (h *serviceTopicHarness) manager() *Manager {
	return &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID: "telegram-main", BotToken: "test", TargetChatID: targetID(-100123),
			ServiceTopicName: "Group support", ServiceTopicIconEmojiID: "777",
		},
		target: forumTarget{chatID: -100123, topology: forumTopologyGroup},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch filepath.Base(req.URL.Path) {
			case "createForumTopic":
				h.created++
				h.missing = false
				id := h.nextID
				h.nextID++
				return telegramResponse(req, fmt.Sprintf(`{"ok":true,"result":{"message_thread_id":%d}}`, id)), nil
			case "editForumTopic":
				var update forumTopicUpdate
				if err := json.NewDecoder(req.Body).Decode(&update); err != nil {
					return nil, err
				}
				h.topicUpdates = append(h.topicUpdates, update)
				if h.missing {
					return telegramResponse(
						req,
						`{"ok":false,"error_code":400,"description":"Bad Request: message thread not found"}`,
					), nil
				}
				return telegramResponse(
					req,
					`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`,
				), nil
			default:
				return telegramResponse(req, `{"ok":true,"result":true}`), nil
			}
		})},
	}
}
