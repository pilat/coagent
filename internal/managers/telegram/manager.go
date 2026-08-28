package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
)

const (
	foldersPerPage       = 10
	maxMessageChunk      = 4000
	maxTopicNameRunes    = 128 // Telegram forum-topic name limit
	reconnectBackoffBase = 3 * time.Second
	reconnectBackoffMax  = 60 * time.Second
)

var _ interface {
	ID() string
	Start(context.Context) error
	Stop(context.Context) error
	Alive() bool
} = (*Manager)(nil)

type Manager struct {
	id             string
	cfg            config.ManagerEntry
	unifiedCfg     *config.UnifiedConfig
	controller     controllerapi.Controller
	httpClient     *http.Client // poll/API calls: bounded by the poll timeout
	downloadClient *http.Client // bot-API file downloads: request-scoped deadlines only

	mu sync.RWMutex

	serviceTopicID  int64
	target          forumTarget
	botUserID       int64
	updateOffset    int64
	daemonHome      string
	availableModels []controllerapi.ConfigModelInfo
	availableSkills []controllerapi.ConfigSkillInfo

	navCounter int64
	navPaths   map[int64]string
	pathToNav  map[string]int64

	sessionToTopic map[int64]int64
	topicToSession map[int64]int64
	workDirs       map[int64]string

	subscription <-chan controllerapi.SessionNotification
	delivery     managerdelivery.Worker
	cancel       context.CancelFunc
	done         chan struct{}
}

type telegramUpdate struct {
	UpdateID      int64                 `json:"update_id"`
	Message       *telegramMessage      `json:"message,omitempty"`
	CallbackQuery *telegramCallbackData `json:"callback_query,omitempty"`
}

type telegramMessage struct {
	MessageID       int64                 `json:"message_id"`
	MessageThreadID int64                 `json:"message_thread_id,omitempty"`
	From            *telegramUser         `json:"from,omitempty"`
	Chat            telegramChat          `json:"chat"`
	Text            string                `json:"text,omitempty"`
	Caption         string                `json:"caption,omitempty"`
	Voice           *telegramVoiceMessage `json:"voice,omitempty"`
	Document        *telegramDocument     `json:"document,omitempty"`
	Photo           []telegramPhotoSize   `json:"photo,omitempty"`
	Video           *telegramVideo        `json:"video,omitempty"`
	Audio           *telegramAudio        `json:"audio,omitempty"`
}

type telegramUser struct {
	ID int64 `json:"id"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

type telegramVoiceMessage struct {
	FileID string `json:"file_id"`
}

// Attachment payloads share a shape: a file_id to resolve via getFile, an
// optional original name (photos have none), optional mime and byte size.
type telegramDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type telegramPhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type telegramVideo struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
	Duration int    `json:"duration,omitempty"`
}

type telegramAudio struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type telegramCallbackData struct {
	ID      string                `json:"id"`
	From    *telegramUser         `json:"from,omitempty"`
	Message *telegramCallbackMeta `json:"message,omitempty"`
	Data    string                `json:"data,omitempty"`
}

type telegramCallbackMeta struct {
	Chat            telegramChat `json:"chat"`
	MessageID       int64        `json:"message_id"`
	MessageThreadID int64        `json:"message_thread_id,omitempty"`
}

type tgReplyMarkup struct {
	InlineKeyboard [][]tgInlineButton `json:"inline_keyboard"`
}

type tgInlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

func New(
	entry config.ManagerEntry,
	unifiedCfg *config.UnifiedConfig,
	controller controllerapi.Controller,
) (*Manager, error) {
	if controller == nil {
		return nil, errors.New("controller is required")
	}

	timeout, err := httpTimeoutFor(entry.PollTimeoutSec)
	if err != nil {
		return nil, fmt.Errorf("manager %q: %w", entry.ID, err)
	}

	m := &Manager{
		id:             entry.ID,
		cfg:            entry,
		unifiedCfg:     unifiedCfg,
		controller:     controller,
		httpClient:     &http.Client{Timeout: timeout},
		downloadClient: &http.Client{}, // request-scoped deadlines: bot-API downloads must outlive PollTimeoutSec
		navPaths:       make(map[int64]string),
		pathToNav:      make(map[string]int64),
		sessionToTopic: make(map[int64]int64),
		topicToSession: make(map[int64]int64),
		workDirs:       make(map[int64]string),
	}

	target, err := m.resolveForumTarget()
	if err != nil {
		return nil, err
	}

	m.target = target

	return m, nil
}

func (m *Manager) ID() string {
	return m.id
}

//nolint:contextcheck,funlen // Startup owns the long-lived context and orders identity, repair, then delivery.
func (m *Manager) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.Background())

	m.cancel = cancel
	if m.target.chatID == 0 {
		target, err := m.resolveForumTarget()
		if err != nil {
			cancel()
			return err
		}

		m.target = target
	}

	// done is read by Alive from whichever goroutine asks for status, so the
	// handoff to the loops is published under the lock.
	done := make(chan struct{})

	started := false
	defer func() {
		if !started {
			cancel()

			if m.subscription != nil {
				m.controller.Unsubscribe(m.subscription)
			}

			close(done)
		}
	}()

	m.mu.Lock()
	m.done = done
	m.mu.Unlock()

	if err := m.preflight(runCtx); err != nil {
		cancel()
		return fmt.Errorf("preflight forum target: %w", err)
	}

	//nolint:contextcheck // runCtx is the manager's own long-lived root context, canceled by Stop, not derived from Start's caller ctx
	serviceTopicID, err := m.ensureServiceTopic(runCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("ensure service topic: %w", err)
	}

	m.serviceTopicID = serviceTopicID

	m.subscription = m.controller.Subscribe()
	if queue, ok := m.controller.(controllerapi.OutputQueueController); ok {
		if err := queue.BindOutputDelivery(runCtx, controllerapi.OutputBindingData{
			Driver: telegramChannel,
			Attributes: map[string]any{
				"bot_user_id": m.botUserID,
				"chat_id":     m.target.chatID,
				"topology":    string(m.target.topology),
			},
		}); err != nil {
			cancel()
			return fmt.Errorf("bind durable output delivery: %w", err)
		}

		var deliveryQueue managerdelivery.Queue = newOutputQueue(queue)
		var transport managerdelivery.Transport = &outputTransport{manager: m}
		m.delivery = managerdelivery.New(deliveryQueue, transport)
	}

	//nolint:contextcheck // same long-lived runCtx as above
	if err := m.reconcileOnStartup(runCtx); err != nil {
		cancel()
		return fmt.Errorf("reconcile sessions: %w", err)
	}

	if progressController, ok := m.controller.(controllerapi.ProgressController); ok {
		sessions, listErr := m.controller.ListSessions(runCtx)
		if listErr != nil {
			cancel()
			return fmt.Errorf("list sessions for progress refresh: %w", listErr)
		}

		for _, session := range sessions {
			if session.HasActiveLoop {
				if refreshErr := progressController.RefreshProgress(runCtx, session.ID); refreshErr != nil {
					cancel()
					return fmt.Errorf("refresh session %d progress: %w", session.ID, refreshErr)
				}
			}
		}
	}

	//nolint:contextcheck // same long-lived runCtx as above
	if err := m.setCommands(runCtx); err != nil {
		cancel()
		return fmt.Errorf("set commands: %w", err)
	}

	if m.delivery != nil {
		m.delivery.Start(runCtx)
	}

	go func() {
		defer close(done)
		defer func() {
			if m.delivery != nil {
				_ = m.delivery.Stop(context.Background())
			}
		}()
		var wg sync.WaitGroup

		wg.Add(2)

		go func() {
			defer wg.Done()

			m.notificationsLoop(runCtx)
		}()
		go func() {
			defer wg.Done()

			m.pollLoop(runCtx)
		}()

		wg.Wait()
	}()

	started = true

	return nil
}

// Alive reports whether the poll and notification loops are still running, so a bot
// that stopped answering stops being reported as running.
func (m *Manager) Alive() bool {
	m.mu.RLock()
	done := m.done
	m.mu.RUnlock()

	if done == nil {
		return false
	}

	select {
	case <-done:
		return false
	default:
		return true
	}
}

func (m *Manager) Stop(ctx context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}

	if m.delivery != nil {
		if err := m.delivery.Stop(ctx); err != nil {
			return fmt.Errorf("stop telegram output delivery: %w", err)
		}
	}

	if m.subscription != nil {
		m.controller.Unsubscribe(m.subscription)
	}

	if m.done == nil {
		return nil
	}

	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) notificationsLoop(ctx context.Context) {
	if m.subscription == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case sn, ok := <-m.subscription:
			if !ok {
				return
			}

			m.handleNotification(ctx, sn)
		}
	}
}
