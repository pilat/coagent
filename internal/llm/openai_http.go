package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

func (c *openaiClient) Close() error {
	return nil
}

func (c *openaiClient) Model() string {
	return c.model
}

func (c *openaiClient) APIKey() string {
	return c.apiKey
}

func (c *openaiClient) ContextWindow() int {
	return c.contextWindow
}

func (c *openaiClient) SetReasoningLevel(level string) {
	c.reasoningLevel = ReasoningLevel(level)
}

func (c *openaiClient) GetReasoningLevel() string {
	if c.reasoningLevel == "" {
		return string(ReasoningMedium)
	}

	return string(c.reasoningLevel)
}

func (c *openaiClient) SetSessionID(id string) {
	// No-op for base openaiClient; openAICompatibleClient overrides when isOpenRouter
}

func (c *openaiClient) makeRequest(ctx context.Context, reqBody oaiRequest) (*llmwire.Response, error) {
	log := logger.Ctx(ctx).Named("llm.client")

	start := time.Now()

	log.Debug(
		"request",
		zap.String("provider", c.provider),
		zap.String("model", c.model),
		zap.Int("messages", len(reqBody.Messages)),
		zap.Int("tools", len(reqBody.Tools)),
	)

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"

	req, err := c.buildHTTPRequest(ctx, url, jsonBody)
	if err != nil {
		return nil, err
	}

	body, err := c.executeHTTPRequest(ctx, log, req)
	if err != nil {
		return nil, err
	}

	var completionResp oaiResponse
	if err := json.Unmarshal(body, &completionResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if len(completionResp.Choices) == 0 {
		return nil, fmt.Errorf("%s api returned no choices", c.provider)
	}

	choice := completionResp.Choices[0]

	message := choice.Message

	result, err := c.parseMessage(&message)
	if err != nil {
		return nil, err
	}

	durationMs := time.Since(start).Milliseconds()
	c.logResponse(log, choice.FinishReason, result, durationMs)

	if err := c.checkEmptyResponse(log, result, choice.FinishReason, body, &completionResp); err != nil {
		return nil, err
	}

	usage := extractUsage(&completionResp, c.provider, c.model, c.pricing)
	attachUsage(result, usage)

	return result, nil
}

func (c *openaiClient) logResponse(log *zap.Logger, finishReason string, result *llmwire.Response, durationMs int64) {
	var textLen int
	if result.Text != "" {
		textLen = len(result.Text)
	}

	log.Debug("response", zap.String("provider", c.provider), zap.String("model", c.model),
		zap.String("finish_reason", finishReason), zap.Int("tool_calls", len(result.ToolCalls)),
		zap.Int("text_len", textLen), zap.Int64("duration_ms", durationMs))
}

func (c *openaiClient) checkEmptyResponse(
	log *zap.Logger,
	result *llmwire.Response,
	finishReason string,
	body []byte,
	completionResp *oaiResponse,
) error {
	if result.Text != "" || len(result.ToolCalls) != 0 {
		return nil
	}

	rawBody := string(body)
	if len(rawBody) > 2000 {
		rawBody = rawBody[:2000] + "..."
	}

	log.Warn("empty_response_body",
		zap.String("provider", c.provider),
		zap.String("finish_reason", finishReason),
		zap.String("raw", rawBody),
	)

	if completionResp.Usage.CompletionTokens == 0 {
		return fmt.Errorf(
			"%s: model returned empty response with 0 completion tokens (possible model overload or incompatibility)",
			c.provider,
		)
	}

	return nil
}

func attachUsage(result *llmwire.Response, usage Usage) {
	result.CostUSD = usage.CostUSD
	result.Usage = &llmwire.MessageUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CacheTokens:      usage.CacheTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
	}
}

func (c *openaiClient) convertMessages(messages []llmwire.Message) []map[string]any {
	var result []map[string]any

	for _, msg := range messages {
		switch msg.Role {
		case roleUser:
			result = append(result, map[string]any{
				msgKeyRole:    roleUser,
				msgKeyContent: msg.Content,
			})
		case roleAssistant:
			var reasoningContent any
			if msg.ReasoningContent != "" {
				reasoningContent = msg.ReasoningContent
			}

			assistantMsg := map[string]any{
				msgKeyRole:          roleAssistant,
				msgKeyContent:       msg.Content,
				"reasoning_content": reasoningContent,
			}

			// OpenRouter requires the response's reasoning_details echoed back
			// verbatim on tool-calling turns, or the next call rejects the message.
			if c.replayReasoning {
				if details, ok := unwrapReasoning(msg.ReasoningRaw, c.model); ok {
					assistantMsg["reasoning_details"] = details
				}
			}

			if len(msg.ToolCalls) > 0 {
				toolCalls := make([]map[string]any, 0, len(msg.ToolCalls))
				for _, tc := range msg.ToolCalls {
					toolCalls = append(toolCalls, map[string]any{
						"id":       tc.ID,
						msgKeyType: oaiTypeFunction,
						oaiTypeFunction: map[string]any{
							"name":      tc.Name,
							"arguments": string(tc.Arguments),
						},
					})
				}

				assistantMsg["tool_calls"] = toolCalls
			}

			result = append(result, assistantMsg)
		case roleTool:
			result = append(result, map[string]any{
				msgKeyRole:     roleTool,
				msgKeyContent:  msg.Content,
				"tool_call_id": msg.ToolCallID,
				"name":         msg.ToolName,
			})
		}
	}

	return result
}

func (c *openaiClient) convertTools(tools []llmwire.ToolSchema) []oaiToolDef {
	if len(tools) == 0 {
		return nil
	}

	result := make([]oaiToolDef, 0, len(tools))

	for _, t := range tools {
		schema, ok := parseParamsSchema(t.Name, t.Parameters)
		if !ok {
			continue
		}

		// OpenAI/Azure requires "properties" in object schemas — MCP tools
		// without parameters return {"type":"object"} which is rejected.
		ensureSchemaProperties(schema)

		result = append(result, oaiToolDef{
			Type: oaiTypeFunction,
			Function: oaiFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}

	return result
}

// buildHTTPRequest creates an HTTP request with auth headers and token refresh.
func (c *openaiClient) buildHTTPRequest(ctx context.Context, url string, jsonBody []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pilat/coagent")
	req.Header.Set("X-Title", "Coagent")
	req.Header.Set("Http-Referer", "https://github.com/pilat/coagent")

	// Use dynamic token from tokenSource (Google SA) if available, otherwise static apiKey
	authToken := c.apiKey

	if c.tokenSource != nil {
		tok, err := c.tokenSource.Token()
		if err != nil {
			return nil, fmt.Errorf("refresh google token: %w", err)
		}

		authToken = tok.AccessToken
	}

	req.Header.Set("Authorization", "Bearer "+authToken)

	return req, nil
}

// executeHTTPRequest sends the request with progress logging and returns the response body.
func (c *openaiClient) executeHTTPRequest(ctx context.Context, log *zap.Logger, req *http.Request) ([]byte, error) {
	deadline, hasDeadline := ctx.Deadline()
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		start := time.Now()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				elapsed := time.Since(start)

				if hasDeadline {
					remaining := time.Until(deadline)
					log.Debug(
						"waiting_for_response",
						zap.Duration("elapsed", elapsed.Round(time.Second)),
						zap.Duration("remaining", remaining.Round(time.Second)),
					)
				} else {
					log.Debug("waiting_for_response", zap.Duration("elapsed", elapsed.Round(time.Second)))
				}
			}
		}
	}()

	resp, err := c.httpClient.Do(req) //nolint:gosec // base URL is operator-configured, not user input

	close(done)

	if err != nil {
		return nil, fmt.Errorf("%s api request failed: %w", c.provider, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s api error: status=%d, body=%s", c.provider, resp.StatusCode, string(body))
	}

	return body, nil
}
