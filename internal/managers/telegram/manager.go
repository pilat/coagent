package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
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
	id         string
	cfg        config.ManagerEntry
	unifiedCfg *config.UnifiedConfig
	controller controllerapi.Controller
	httpClient *http.Client

	mu sync.RWMutex

	serviceTopicID  int64
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
	Voice           *telegramVoiceMessage `json:"voice,omitempty"`
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

	m := &Manager{
		id:             entry.ID,
		cfg:            entry,
		unifiedCfg:     unifiedCfg,
		controller:     controller,
		httpClient:     &http.Client{Timeout: 45 * time.Second},
		navPaths:       make(map[int64]string),
		pathToNav:      make(map[string]int64),
		sessionToTopic: make(map[int64]int64),
		topicToSession: make(map[int64]int64),
		workDirs:       make(map[int64]string),
	}

	return m, nil
}

func (m *Manager) ID() string {
	return m.id
}

//nolint:contextcheck // Start spawns runCtx as the manager's own long-lived root context, canceled by Stop, not derived from the caller's ctx
func (m *Manager) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	// done is read by Alive from whichever goroutine asks for status, so the
	// handoff to the loops is published under the lock.
	done := make(chan struct{})

	m.mu.Lock()
	m.done = done
	m.mu.Unlock()

	//nolint:contextcheck // runCtx is the manager's own long-lived root context, canceled by Stop, not derived from Start's caller ctx
	serviceTopicID, err := m.ensureServiceTopic(runCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("ensure service topic: %w", err)
	}

	m.serviceTopicID = serviceTopicID

	//nolint:contextcheck // same long-lived runCtx as above
	if err := m.reconcileOnStartup(runCtx); err != nil {
		cancel()
		return fmt.Errorf("reconcile sessions: %w", err)
	}

	//nolint:contextcheck // same long-lived runCtx as above
	if err := m.setCommands(runCtx); err != nil {
		cancel()
		return fmt.Errorf("set commands: %w", err)
	}

	m.subscription = m.controller.Subscribe()

	go func() {
		defer close(done)
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

func (m *Manager) pollLoop(ctx context.Context) {
	log := logger.Ctx(ctx).Named("telegram.poll")
	backoff := reconnectBackoffBase
	fatalWarned := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, err := m.getUpdates(ctx, m.updateOffset)
		if err != nil {
			wait := nextPollWait(err, &backoff, &fatalWarned, log)

			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				continue
			}
		}

		backoff = reconnectBackoffBase
		fatalWarned = false

		for _, upd := range updates {
			m.updateOffset = upd.UpdateID + 1
			m.processUpdate(ctx, upd)
		}
	}
}

// nextPollWait picks how long to wait after a getUpdates failure and advances the
// exponential backoff. A 429 is honored to the second; a fatal error that won't
// self-heal (bad token, conflicting poller) throttles to the max and warns once;
// every other failure backs off exponentially. Retries are never capped — the bot
// must recover whenever the outage clears.
func nextPollWait(err error, backoff *time.Duration, fatalWarned *bool, log *zap.Logger) time.Duration {
	var apiErr *tgAPIError
	if errors.As(err, &apiErr) {
		if apiErr.RetryAfter > 0 {
			log.Warn("getupdates_rate_limited", zap.Int("retry_after", apiErr.RetryAfter))

			return time.Duration(apiErr.RetryAfter) * time.Second
		}

		if isFatalPollError(apiErr.ErrorCode) {
			if !*fatalWarned {
				*fatalWarned = true

				log.Error("getupdates_fatal",
					zap.Int("error_code", apiErr.ErrorCode),
					zap.String("description", apiErr.Description))
			}

			*backoff = reconnectBackoffMax

			return *backoff
		}
	}

	log.Warn("getupdates_failed", zap.Duration("retry_in", *backoff), zap.Error(err))

	wait := *backoff
	*backoff = min(*backoff*2, reconnectBackoffMax)

	return wait
}

// isFatalPollError reports telegram error codes that will not self-heal by
// retrying: bad token (401), forbidden (403), or a conflicting poller/webhook (409).
func isFatalPollError(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusConflict
}

func (m *Manager) processUpdate(ctx context.Context, upd telegramUpdate) {
	if upd.CallbackQuery != nil {
		m.handleCallback(ctx, upd.CallbackQuery)
		return
	}

	log := logger.Ctx(ctx).Named("telegram")

	msg := upd.Message
	if msg == nil {
		log.Debug("update_dropped", zap.String("reason", "not_a_message"))
		return
	}

	if msg.Text == "" && msg.Voice == nil {
		log.Debug("update_dropped", zap.String("reason", "no_text_or_voice"))
		return
	}

	if msg.Chat.ID != m.cfg.TargetChatID {
		log.Debug("update_dropped", zap.String("reason", "other_chat"), zap.Int64("chat_id", msg.Chat.ID))
		return
	}

	if msg.From == nil || !m.isAllowedUser(msg.From.ID) {
		log.Debug("update_dropped", zap.String("reason", "user_not_allowed"))
		return
	}

	threadID := msg.MessageThreadID
	if threadID == 0 {
		log.Debug("update_dropped", zap.String("reason", "no_topic"))
		return
	}

	sessionID, hasSession := m.resolveSessionByTopicID(ctx, threadID)

	log.Info("message_received",
		zap.Int64("thread_id", threadID),
		zap.Int64("session_id", sessionID),
		zap.Bool("has_session", hasSession),
		zap.String("text", textPreview(msg.Text)))

	if msg.Voice != nil {
		m.handleVoiceMessage(ctx, msg, threadID, sessionID, hasSession)
		return
	}

	text := normalizeTextCommand(msg.Text)
	if threadID == m.serviceTopicID {
		m.handleServiceTopicMessage(ctx, text)
		return
	}

	if hasSession {
		m.handleSessionTopicMessage(ctx, sessionID, threadID, text)
		return
	}

	log.Info("update_dropped", zap.String("reason", "no_session_for_topic"), zap.Int64("thread_id", threadID))
}

// textPreview truncates a message body for log lines — enough to recognize the
// message, not carry the whole payload into logs.
func textPreview(s string) string {
	const limit = 48

	r := []rune(s)
	if len(r) <= limit {
		return s
	}

	return string(r[:limit]) + "…"
}

func (m *Manager) isAllowedUser(userID int64) bool {
	return slices.Contains(m.cfg.AllowedUserIDs, userID)
}
