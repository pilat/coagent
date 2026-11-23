package telegram

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/managerdelivery"
)

const (
	maxMessageChunk      = 4000
	reconnectBackoffBase = 3 * time.Second
	reconnectBackoffMax  = 60 * time.Second
	startupRetryTimeout  = 30 * time.Second
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
	api            apiClients
	httpClient     *http.Client // poll/API calls: bounded by the poll timeout
	downloadClient *http.Client // bot-API file downloads: request-scoped deadlines only

	mu sync.RWMutex

	chatID       int64
	updateOffset int64
	workDirs     map[int64]string

	delivery managerdelivery.ChatLoop
	cancel   context.CancelFunc
	done     chan struct{}
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message,omitempty"`
}

type telegramMessage struct {
	MessageID int64                 `json:"message_id"`
	From      *telegramUser         `json:"from,omitempty"`
	Chat      telegramChat          `json:"chat"`
	Text      string                `json:"text,omitempty"`
	Caption   string                `json:"caption,omitempty"`
	Voice     *telegramVoiceMessage `json:"voice,omitempty"`
	Document  *telegramDocument     `json:"document,omitempty"`
	Photo     []telegramPhotoSize   `json:"photo,omitempty"`
	Video     *telegramVideo        `json:"video,omitempty"`
	Audio     *telegramAudio        `json:"audio,omitempty"`
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

func New(
	entry config.ManagerEntry,
	unifiedCfg *config.UnifiedConfig,
	transport connect.Transport,
) (*Manager, error) {
	timeout, err := httpTimeoutFor(entry.PollTimeoutSec)
	if err != nil {
		return nil, fmt.Errorf("manager %q: %w", entry.ID, err)
	}

	m := &Manager{
		id:             entry.ID,
		cfg:            entry,
		unifiedCfg:     unifiedCfg,
		api:            newAPI(transport),
		httpClient:     &http.Client{Timeout: timeout},
		downloadClient: &http.Client{}, // request-scoped deadlines: bot-API downloads must outlive PollTimeoutSec
		workDirs:       make(map[int64]string),
	}

	chatID, err := m.resolveChatID()
	if err != nil {
		return nil, err
	}

	m.chatID = chatID

	return m, nil
}

func (m *Manager) ID() string {
	return m.id
}

//nolint:contextcheck // Failed startup cleanup must outlive the cancelled run context.
func (m *Manager) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	m.mu.Lock()
	m.cancel, m.done = cancel, done
	m.mu.Unlock()

	started := false
	defer func() {
		if !started {
			cancel()

			close(done)
		}
	}()

	if err := m.preflightWithRetry(runCtx, defaultStartupRetryPolicy()); err != nil {
		return fmt.Errorf("preflight chat: %w", err)
	}

	if _, err := m.api.chats.BindChat(
		runCtx,
		&coagentv1.BindChatRequest{ChatId: strconv.FormatInt(m.effectiveChatID(), 10)},
	); err != nil {
		return fmt.Errorf("bind chat: %w", err)
	}

	events, err := m.api.outputs.StreamSessionEvents(runCtx, &coagentv1.StreamSessionEventsRequest{})
	if err != nil {
		return fmt.Errorf("stream session events: %w", err)
	}

	if _, err := events.Receive(); err != nil {
		return fmt.Errorf("stream session events: %w", err)
	}

	if err := m.tg(runCtx, "setMyCommands", map[string]any{"commands": publishedCommands}, nil); err != nil {
		return fmt.Errorf("set commands: %w", err)
	}

	var port managerdelivery.ChatPort = &chatPort{
		api: m.api, chatID: strconv.FormatInt(m.effectiveChatID(), 10),
	}
	var transport managerdelivery.ChatTransport = &outputTransport{manager: m}
	delivery := managerdelivery.NewChat(port, transport)

	m.mu.Lock()
	m.delivery = delivery
	m.mu.Unlock()
	delivery.Start(runCtx)

	go func() {
		defer close(done)
		defer func() { _ = delivery.Stop(context.Background()) }()

		var wg sync.WaitGroup

		wg.Go(func() { m.notificationsLoop(runCtx, events) })
		wg.Go(func() { m.pollLoop(runCtx) })
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
	m.mu.RLock()
	cancel := m.cancel
	delivery := m.delivery
	done := m.done
	m.mu.RUnlock()

	if cancel != nil {
		cancel()
	}

	if delivery != nil {
		if err := delivery.Stop(ctx); err != nil {
			return fmt.Errorf("stop telegram output delivery: %w", err)
		}
	}

	if done == nil {
		return nil
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
