package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

const directOpenAIHost = "api.openai.com"

var _ Client = (*openAIResponsesClient)(nil)

// openAIResponsesClient speaks the Responses API to api.openai.com, sharing the
// openaiClient shell for auth, transport, limits and pricing.
type openAIResponsesClient struct {
	openaiClient
}

// isDirectOpenAI reports whether a provider's base URL is OpenAI itself. Client
// construction and the effort allowlist both decide by it, so they never disagree.
func isDirectOpenAI(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}

	return strings.EqualFold(u.Hostname(), directOpenAIHost)
}

func newOpenAIResponsesClient(baseURL, apiKey string, model config.ModelEntry) (Client, error) {
	if apiKey == "" {
		return nil, errors.New("API key is required")
	}

	if baseURL == "" {
		return nil, errors.New("base URL is required for OpenAI-compatible provider")
	}

	if model.ID == "" {
		return nil, errors.New("model is required")
	}

	return &openAIResponsesClient{
		openaiClient: newOpenAIClient(baseURL, apiKey, driverOpenAI, nil, model),
	}, nil
}

func (c *openAIResponsesClient) Provider() string {
	return c.provider
}

func (c *openAIResponsesClient) Chat(
	ctx context.Context,
	systemPrompt string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	return c.send(ctx, c.buildRequest(systemPrompt, messages, tools, opts))
}

func (c *openAIResponsesClient) buildRequest(
	systemPrompt string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts []llmwire.ChatOption,
) responsesRequest {
	req := responsesRequest{
		Model:           c.model,
		Instructions:    systemPrompt,
		Input:           c.convertInput(messages),
		MaxOutputTokens: llmwire.ApplyChatOptions(opts).EffectiveMaxTokens(c.maxTokens),
	}

	if defs := c.convertResponsesTools(tools); len(defs) > 0 {
		req.Tools = defs
		req.ToolChoice = "auto"
	}

	if c.reasoningCapable() {
		req.Include = []string{respIncludeEncrypted}
	}

	if effort, ok := c.effortParam(); ok {
		req.Reasoning = &responsesReasoning{Effort: effort}
	}

	return req
}

// reasoningCapable gates encrypted reasoning: the API rejects the include on
// models that do not reason, and every api.openai.com model lands here.
func (c *openAIResponsesClient) reasoningCapable() bool {
	return c.reasoning != nil && c.reasoning.Supported
}

func (c *openAIResponsesClient) effortParam() (string, bool) {
	if !c.reasoningCapable() || len(c.reasoning.Efforts) == 0 {
		return "", false
	}

	return catalog.ClampEffort(c.GetReasoningLevel(), c.reasoning.Efforts), true
}

func (c *openAIResponsesClient) convertResponsesTools(tools []llmwire.ToolSchema) []responsesTool {
	result := make([]responsesTool, 0, len(tools))

	for _, t := range tools {
		schema, ok := parseParamsSchema(t.Name, t.Parameters)
		if !ok {
			continue
		}

		ensureSchemaProperties(schema)

		result = append(result, responsesTool{
			Type:        oaiTypeFunction,
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		})
	}

	return result
}

func (c *openAIResponsesClient) send(ctx context.Context, reqBody responsesRequest) (*llmwire.Response, error) {
	log := logger.Ctx(ctx).Named("llm.client")

	start := time.Now()

	log.Debug(
		"request",
		zap.String("provider", c.provider),
		zap.String("model", c.model),
		zap.Int("input_items", len(reqBody.Input)),
		zap.Int("tools", len(reqBody.Tools)),
	)

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := c.buildHTTPRequest(ctx, c.baseURL+"/responses", jsonBody)
	if err != nil {
		return nil, err
	}

	trace := newRequestTrace(len(jsonBody))
	req = trace.attach(req)

	body, err := c.executeHTTPRequest(ctx, log, req)
	logRequestTrace(log, trace, err)

	if err != nil {
		return nil, err
	}

	return c.parseResponsesBody(log, body, start)
}
