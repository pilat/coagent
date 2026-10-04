package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionstore"
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
		if len(calls) > 0 {
			finish = "tool_calls"
		}
		if response.FinishType == llmwire.FinishLength {
			finish = "length"
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
