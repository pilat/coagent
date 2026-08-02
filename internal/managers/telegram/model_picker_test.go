package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

type tgCall struct {
	Method string
	Text   string
	Markup *tgReplyMarkup
}

func pickerManager(t *testing.T, ctrl *fakeController, calls *[]tgCall) *Manager {
	t.Helper()

	enabled := true

	return &Manager{
		id: "telegram-main",
		cfg: config.ManagerEntry{
			ID: "telegram-main", Enabled: &enabled, BotToken: "token",
			TargetChatID: -100123, PollTimeoutSec: 30,
		},
		controller:     ctrl,
		httpClient:     &http.Client{Transport: telegramCallRecorder(t, calls)},
		navPaths:       map[int64]string{},
		pathToNav:      map[string]int64{},
		sessionToTopic: map[int64]int64{},
		topicToSession: map[int64]int64{},
		workDirs:       map[int64]string{},
	}
}

func telegramCallRecorder(t *testing.T, calls *[]tgCall) roundTripFunc {
	t.Helper()

	return func(req *http.Request) (*http.Response, error) {
		var body struct {
			Text        string          `json:"text"`
			ReplyMarkup *tgReplyMarkup  `json:"reply_markup"`
			Raw         json.RawMessage `json:"-"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))

		parts := strings.Split(req.URL.Path, "/")
		*calls = append(*calls, tgCall{Method: parts[len(parts)-1], Text: body.Text, Markup: body.ReplyMarkup})

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":123}}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
}

func buttonTexts(markup *tgReplyMarkup) []string {
	var texts []string

	if markup == nil {
		return texts
	}

	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			texts = append(texts, b.Text)
		}
	}

	return texts
}

func buttonData(markup *tgReplyMarkup) []string {
	var data []string

	if markup == nil {
		return data
	}

	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			data = append(data, b.CallbackData)
		}
	}

	return data
}

var pickerModels = []controllerapi.ConfigModelInfo{
	{
		ID: "claude-opus-5", Name: "Claude Opus 5", DisplayName: "anthropic/Claude Opus 5",
		InputPrice: 5, OutputPrice: 25,
		EffortLevels: []string{"low", "medium", "high", "max"}, DefaultEffort: "medium",
	},
	{
		ID: "local/plain", Name: "Plain", DisplayName: "local/Plain",
		InputPrice: 0, OutputPrice: 0,
	},
}

func TestHandleModelRendersDisplayNamePriceAndEffort(t *testing.T) {
	ctrl := &fakeController{
		listModels: pickerModels,
		listSessions: []controllerapi.SessionInfo{
			{ID: 1, Model: "claude-opus-5", ReasoningLevel: "high"},
		},
	}

	var calls []tgCall

	m := pickerManager(t, ctrl, &calls)
	m.handleModel(context.Background(), 1, 900)

	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].Text, "anthropic/Claude Opus 5")
	assert.Contains(t, calls[0].Text, "effort: high")
	assert.Equal(t, []string{"✅ anthropic/Claude Opus 5 · $5/$25", "local/Plain · free"}, buttonTexts(calls[0].Markup))
	assert.Equal(t, []string{"model:claude-opus-5", "model:local/plain"}, buttonData(calls[0].Markup))
}

// A model with no effort choice must not claim one: showing a level nobody honours
// is exactly the lie the catalog-driven list exists to remove.
func TestHandleModelOmitsEffortWhenSessionHasNone(t *testing.T) {
	ctrl := &fakeController{
		listModels:   pickerModels,
		listSessions: []controllerapi.SessionInfo{{ID: 1, Model: "local/plain"}},
	}

	var calls []tgCall

	m := pickerManager(t, ctrl, &calls)
	m.handleModel(context.Background(), 1, 900)

	require.Len(t, calls, 1)
	assert.NotContains(t, calls[0].Text, "effort:")
}

func TestCallbackModelResetsEffortAndOffersTheEffortStep(t *testing.T) {
	ctrl := &fakeController{listModels: pickerModels}

	var calls []tgCall

	m := pickerManager(t, ctrl, &calls)
	m.registerTopic(7, 900)
	m.availableModels = pickerModels

	m.handleCallbackModel(context.Background(), &telegramCallbackData{
		ID: "cb", Message: &telegramCallbackMeta{MessageID: 42},
	}, callbackAction{Kind: callbackModel, ModelID: "claude-opus-5"}, 900)

	require.Len(t, ctrl.setModelCalls, 1)
	assert.Equal(t, int64(7), ctrl.setModelCalls[0].SessionID)
	assert.Equal(t, "claude-opus-5", ctrl.setModelCalls[0].Model)
	assert.Equal(t, "medium", ctrl.setModelCalls[0].ReasoningLevel)

	edit := calls[len(calls)-1]
	assert.Equal(t, "editMessageText", edit.Method)
	assert.Contains(t, edit.Text, "Pick reasoning effort")
	assert.Equal(t, []string{"⚡️ Low", "✅ Medium", "🔥 High", "🧠 Max"}, buttonTexts(edit.Markup),
		"the keyboard mirrors the model's own allowlist, not a fixed trio")
	assert.Equal(t, []string{
		"effort:claude-opus-5:low",
		"effort:claude-opus-5:medium",
		"effort:claude-opus-5:high",
		"effort:claude-opus-5:max",
	}, buttonData(edit.Markup))
}

// A narrow allowlist must produce a narrow keyboard — no dead buttons for levels
// the gateway would only remap behind our back.
func TestCallbackModelOffersOnlyTheLevelsTheModelAccepts(t *testing.T) {
	narrow := []controllerapi.ConfigModelInfo{{
		ID: "z-ai/glm-narrow", DisplayName: "openrouter/GLM Narrow",
		EffortLevels: []string{"high", "xhigh"}, DefaultEffort: "xhigh",
	}}
	ctrl := &fakeController{listModels: narrow}

	var calls []tgCall

	m := pickerManager(t, ctrl, &calls)
	m.registerTopic(7, 900)
	m.availableModels = narrow

	m.handleCallbackModel(context.Background(), &telegramCallbackData{
		ID: "cb", Message: &telegramCallbackMeta{MessageID: 42},
	}, callbackAction{Kind: callbackModel, ModelID: "z-ai/glm-narrow"}, 900)

	require.Len(t, ctrl.setModelCalls, 1)
	assert.Equal(t, "xhigh", ctrl.setModelCalls[0].ReasoningLevel,
		"a switch lands on the model's own default, not a global medium")

	edit := calls[len(calls)-1]
	assert.Equal(t, []string{"🔥 High", "✅ 🔥🔥 X-High"}, buttonTexts(edit.Markup))
}

// A model that cannot reason must behave exactly as before: the check mark moves
// and the model keyboard stays put.
func TestCallbackModelOnNonReasoningModelKeepsTheModelKeyboard(t *testing.T) {
	ctrl := &fakeController{listModels: pickerModels}

	var calls []tgCall

	m := pickerManager(t, ctrl, &calls)
	m.registerTopic(7, 900)
	m.availableModels = pickerModels

	m.handleCallbackModel(context.Background(), &telegramCallbackData{
		ID: "cb", Message: &telegramCallbackMeta{MessageID: 42},
	}, callbackAction{Kind: callbackModel, ModelID: "local/plain"}, 900)

	edit := calls[len(calls)-1]
	assert.Contains(t, edit.Text, "Switched for current session")
	assert.Equal(t, []string{"anthropic/Claude Opus 5 · $5/$25", "✅ local/Plain · free"}, buttonTexts(edit.Markup))
}

func TestCallbackModelSurfacesSwitchFailure(t *testing.T) {
	ctrl := &fakeController{listModels: pickerModels, setModelErr: errors.New("boom")}

	var calls []tgCall

	m := pickerManager(t, ctrl, &calls)
	m.registerTopic(7, 900)
	m.availableModels = pickerModels

	m.handleCallbackModel(context.Background(), &telegramCallbackData{
		ID: "cb", Message: &telegramCallbackMeta{MessageID: 42},
	}, callbackAction{Kind: callbackModel, ModelID: "claude-opus-5"}, 900)

	require.Len(t, calls, 1)
	assert.Equal(t, "answerCallbackQuery", calls[0].Method)
	assert.NotContains(t, calls[0].Text, "Switched")
}

func TestCallbackModelRefetchesModelsWhenCacheIsEmpty(t *testing.T) {
	ctrl := &fakeController{listModels: pickerModels}

	var calls []tgCall

	m := pickerManager(t, ctrl, &calls)
	m.registerTopic(7, 900)

	m.handleCallbackModel(context.Background(), &telegramCallbackData{
		ID: "cb", Message: &telegramCallbackMeta{MessageID: 42},
	}, callbackAction{Kind: callbackModel, ModelID: "claude-opus-5"}, 900)

	assert.Equal(t, 1, ctrl.listModelsCalls)
	assert.Contains(t, calls[len(calls)-1].Text, "Pick reasoning effort")
}

func TestCallbackEffortAppliesTheLevelAndClearsTheKeyboard(t *testing.T) {
	ctrl := &fakeController{listModels: pickerModels}

	var calls []tgCall

	m := pickerManager(t, ctrl, &calls)
	m.registerTopic(7, 900)
	m.availableModels = pickerModels

	m.handleCallbackEffort(context.Background(), &telegramCallbackData{
		ID: "cb", Message: &telegramCallbackMeta{MessageID: 42},
	}, callbackAction{Kind: callbackEffort, ModelID: "claude-opus-5", Effort: "high"}, 900)

	require.Len(t, ctrl.setModelCalls, 1)
	assert.Equal(t, "claude-opus-5", ctrl.setModelCalls[0].Model)
	assert.Equal(t, "high", ctrl.setModelCalls[0].ReasoningLevel)

	edit := calls[len(calls)-1]
	assert.Equal(t, "editMessageText", edit.Method)
	assert.Contains(t, edit.Text, "effort: high")
	assert.Nil(t, edit.Markup)
}

func TestParseEffortCallback(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantID  string
		wantLvl string
		wantOK  bool
	}{
		{name: "plain", data: "effort:claude-opus-5:low", wantID: "claude-opus-5", wantLvl: "low", wantOK: true},
		{
			name:    "model id with a colon",
			data:    "effort:qwen3:14b:high",
			wantID:  "qwen3:14b",
			wantLvl: "high",
			wantOK:  true,
		},
		{name: "unknown level", data: "effort:claude-opus-5:turbo", wantOK: false},
		{name: "no level", data: "effort:claude-opus-5", wantOK: false},
		{name: "empty level", data: "effort:claude-opus-5:", wantOK: false},
		{name: "no model", data: "effort::low", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCallbackData(tt.data)
			assert.Equal(t, tt.wantOK, ok)

			if !tt.wantOK {
				return
			}

			assert.Equal(t, callbackEffort, got.Kind)
			assert.Equal(t, tt.wantID, got.ModelID)
			assert.Equal(t, tt.wantLvl, got.Effort)
		})
	}
}

func TestFormatPrice(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{3, "3"},
		{15, "15"},
		{0.5, "0.5"},
		{0.25, "0.25"},
		{3.75, "3.75"},
		{0.125, "0.13"},
		{0.004, "0.004"},
		{0.0001, "0"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, formatPrice(tt.in))
		})
	}
}

func TestModelPriceTag(t *testing.T) {
	assert.Equal(t, "free", modelPriceTag(controllerapi.ConfigModelInfo{}))
	assert.Equal(t, "$0/$2", modelPriceTag(controllerapi.ConfigModelInfo{OutputPrice: 2}))
	assert.Equal(t, "$0.8/$4", modelPriceTag(controllerapi.ConfigModelInfo{InputPrice: 0.8, OutputPrice: 4}))
}

func TestModelDisplayNameFallsBack(t *testing.T) {
	assert.Equal(
		t,
		"p/Name",
		modelDisplayName(controllerapi.ConfigModelInfo{ID: "x", Name: "Name", DisplayName: "p/Name"}),
	)
	assert.Equal(t, "Name", modelDisplayName(controllerapi.ConfigModelInfo{ID: "x", Name: "Name"}))
	assert.Equal(t, "x", modelDisplayName(controllerapi.ConfigModelInfo{ID: "x"}))
}
