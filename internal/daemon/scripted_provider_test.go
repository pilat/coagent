package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool/builtin"
)

type providerRequest struct {
	Model     string            `json:"model"`
	SessionID string            `json:"session_id"`
	Messages  []providerMessage `json:"messages"`
	Tools     []struct {
		Function llmwire.ToolSchema `json:"function"`
	} `json:"tools"`
}

type providerMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func scriptedBuildInput(
	t *testing.T,
	cfg *config.Config,
	store *sessionstore.Store,
	mcp mcpstore.Store,
	clientFor func(*config.Config) (llm.Client, error),
) sessionbuild.BuildInput {
	t.Helper()
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	var mu sync.Mutex
	clients := make(map[string]llm.Client)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request providerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		client := clients[request.SessionID]
		if client == nil {
			view := *cfg
			view.Model = request.Model
			var err error
			client, err = clientFor(&view)
			if err != nil {
				mu.Unlock()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			client.SetSessionID(request.SessionID)
			clients[request.SessionID] = client
		}
		mu.Unlock()
		var system string
		var messages []llmwire.Message
		for _, row := range request.Messages {
			var content string
			if err := json.Unmarshal(row.Content, &content); err != nil {
				var parts []struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(row.Content, &parts)
				for _, part := range parts {
					content += part.Text
				}
			}
			if row.Role == "system" {
				system = content
				continue
			}
			message := llmwire.Message{Role: row.Role, Content: content, ToolCallID: row.ToolCallID, ToolName: row.Name}
			for _, call := range row.ToolCalls {
				message.ToolCalls = append(
					message.ToolCalls,
					llmwire.ToolCall{
						ID:        call.ID,
						Name:      call.Function.Name,
						Arguments: json.RawMessage(call.Function.Arguments),
					},
				)
			}
			messages = append(messages, message)
		}
		var schemas []llmwire.ToolSchema
		for _, entry := range request.Tools {
			schemas = append(schemas, entry.Function)
		}
		response, err := client.Chat(r.Context(), system, messages, schemas)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var calls []map[string]any
		for index, call := range response.ToolCalls {
			calls = append(
				calls,
				map[string]any{
					"index":    index,
					"id":       call.ID,
					"type":     "function",
					"function": map[string]any{"name": call.Name, "arguments": string(call.Arguments)},
				},
			)
		}
		finish := "stop"
		if response.FinishType == llmwire.FinishToolCalls || len(calls) > 0 {
			finish = "tool_calls"
		}
		if response.FinishType == llmwire.FinishLength {
			finish = "length"
		}
		if response.FinishType == llmwire.FinishUnknown {
			finish = response.ProviderFinishReason
			if finish == "" {
				finish = "unknown"
			}
		}
		usage := map[string]any{"cost": response.CostUSD}
		if response.Usage != nil {
			usage["prompt_tokens"] = response.Usage.PromptTokens
			usage["completion_tokens"] = response.Usage.CompletionTokens
		}
		body, marshalErr := json.Marshal(
			map[string]any{
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"content": response.Text, "tool_calls": calls},
						"finish_reason": finish,
					},
				},
				"usage": usage,
			},
		)
		if marshalErr != nil {
			http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
	}))
	t.Cleanup(server.Close)
	if cfg.UnifiedConfig == nil {
		cfg.UnifiedConfig = &config.UnifiedConfig{}
	}
	if cfg.UnifiedConfig.Providers == nil {
		cfg.UnifiedConfig.Providers = make(map[string]config.ProviderEntry)
	}
	cfg.UnifiedConfig.Providers["scripted"] = config.ProviderEntry{
		Driver:  "openrouter",
		APIKey:  "fixture",
		BaseURL: server.URL,
	}
	if len(cfg.UnifiedConfig.Models) == 0 {
		cfg.UnifiedConfig.Models = []config.ModelEntry{{ID: cfg.Model, ContextWindow: 200000}}
	}
	for index := range cfg.UnifiedConfig.Models {
		cfg.UnifiedConfig.Models[index].Provider = "scripted"
	}
	return sessionbuild.BuildInput{Config: cfg, Store: store, MCPStore: mcp, Resources: builtin.NewResources()}
}

func newScenarioDaemon(
	ctx context.Context,
	in sessionbuild.BuildInput,
	store *sessionstore.Store,
	links subagent.Store,
	budgets budget.Service,
	schedules schedule.Service,
	model func() string,
	db *sql.DB,
) (*svc, backgroundprocess.Service) {
	bus := sessionbus.New()
	defaultModel := "fake-model"
	if model != nil {
		defaultModel = model()
	}
	if budgets == nil {
		budgets = budget.New(store)
	}
	if schedules == nil {
		schedules = schedule.NewService(schedule.NewStore(db, store), store)
	}
	in.Config.Model = defaultModel
	mcp := in.MCPStore
	if mcp == nil {
		mcp = mcpstore.NewStore(db)
	}
	in.MCPStore = mcp
	applier := configapply.New(
		configops.New(filepath.Join(in.Config.WorkDir, "config.yaml"), filepath.Join(in.Config.WorkDir, "secrets")),
		store,
	)
	service := New(
		ctx,
		in,
		store,
		links,
		budgets,
		backgroundprocess.NewStore(db, store),
		progressruntime.New(store, bus),
		bus,
		schedules,
		in.Config,
		mcp,
		applier,
	).(*svc)
	return service, service.processes
}

func lifecycleInput(ctx context.Context, t *testing.T, s *svc, id int64, command string) *sessionstore.InboxInput {
	t.Helper()
	input, err := s.enqueueUserSessionInput(ctx, id, command)
	require.NoError(t, err)
	return input
}

func enqueueScheduledInput(ctx context.Context, store Store, id int64, key, content string, fresh bool) (bool, error) {
	result, err := store.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:   id,
			Source:      sessionstore.InputSourceSchedule,
			Content:     content,
			DeliveryKey: key,
			Attributes:  map[string]any{"fresh": fresh},
		},
	)
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}

func enqueueCallResult(ctx context.Context, store Store, id int64, callID, name, content string) (bool, error) {
	key := "result:" + callID
	if name == "config_edit" {
		key = "config_apply:" + callID
	}
	result, err := store.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:   id,
			Source:      sessionstore.InputSourceCallResult,
			Content:     content,
			Attributes:  map[string]any{"call_id": callID, "tool_id": name},
			DeliveryKey: key,
		},
	)
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}
