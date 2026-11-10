package llm

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
)

const (
	rsItem1  = `{"type":"reasoning","id":"rs_1","encrypted_content":"ENC1","summary":[]}`
	rsItem2  = `{"type":"reasoning","id":"rs_2","encrypted_content":"ENC2","summary":[]}`
	fcItem1  = `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"p\":1}","status":"completed"}`
	fcItem2  = `{"type":"function_call","id":"fc_2","call_id":"call_2","name":"grep","arguments":"{}","status":"completed"}`
	msgItem1 = `{"type":"message","id":"msg_1","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"done","annotations":[]}]}`
)

func responsesBody(status, extra string, items ...string) []byte {
	output := "[" + strings.Join(items, ",") + "]"

	return []byte(`{"status":"` + status + `","output":` + output + extra +
		`,"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":10,"total_tokens":110}}`)
}

func parseResponses(t *testing.T, c *openAIResponsesClient, body []byte) *llmwire.Response {
	t.Helper()

	resp, err := c.parseResponsesBody(zap.NewNop(), body, time.Now())
	require.NoError(t, err)

	return resp
}

func skeletonOf(t *testing.T, raw json.RawMessage, model string) responsesSkeleton {
	t.Helper()

	payload, ok := unwrapReasoning(raw, model)
	require.True(t, ok)

	var skeleton responsesSkeleton
	require.NoError(t, json.Unmarshal(payload, &skeleton))
	assert.Equal(t, responsesSkeletonFormat, skeleton.Format)

	return skeleton
}

func TestResponsesParseReasoningAndCalls(t *testing.T) {
	c := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())

	resp := parseResponses(t, c, responsesBody("completed", "", rsItem1, fcItem1, fcItem2))

	assert.Equal(t, llmwire.FinishToolCalls, resp.FinishType)
	assert.Equal(t, "completed", resp.ProviderFinishReason)
	assert.Equal(t, []llmwire.ToolCall{
		{ID: "call_1", Name: "read", Arguments: []byte(`{"p":1}`)},
		{ID: "call_2", Name: "grep", Arguments: []byte(`{}`)},
	}, resp.ToolCalls)

	skeleton := skeletonOf(t, resp.ReasoningRaw, "gpt-6-luna")
	require.Len(t, skeleton.Items, 3)
	assert.Equal(t, "reasoning", skeleton.Items[0].Kind)
	assert.JSONEq(t, rsItem1, string(skeleton.Items[0].Raw))
	assert.Equal(t, skeletonEntry{Kind: "function_call", ID: "fc_1", CallID: "call_1"}, skeleton.Items[1])
	assert.Equal(t, skeletonEntry{Kind: "function_call", ID: "fc_2", CallID: "call_2"}, skeleton.Items[2])
}

func TestResponsesParseReasoningAndMessage(t *testing.T) {
	c := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())

	resp := parseResponses(t, c, responsesBody("completed", "", rsItem1, msgItem1))

	assert.Equal(t, "done", resp.Text)
	assert.Equal(t, llmwire.FinishStop, resp.FinishType)

	skeleton := skeletonOf(t, resp.ReasoningRaw, "gpt-6-luna")
	require.Len(t, skeleton.Items, 2)
	assert.JSONEq(t, msgItem1, string(skeleton.Items[1].Raw), "message is stored verbatim")
}

func TestResponsesParseStoresNoEnvelope(t *testing.T) {
	c := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())

	for name, items := range map[string][]string{
		"message only":          {msgItem1},
		"reasoning only":        {rsItem1},
		"unknown between":       {rsItem1, `{"type":"web_search_call","id":"ws_1"}`, fcItem1},
		"trailing reasoning":    {msgItem1, rsItem2},
		"undecodable trailing":  {rsItem1, fcItem1, `"junk"`},
		"unknown after a call":  {rsItem1, fcItem1, `{"type":"web_search_call","id":"ws_1"}`},
		"unencrypted reasoning": {`{"type":"reasoning","id":"rs_x","summary":[]}`, fcItem1},
	} {
		resp := parseResponses(t, c, responsesBody("completed", "", items...))
		assert.Empty(t, resp.ReasoningRaw, name)
	}

	resp := parseResponses(
		t,
		c,
		responsesBody("completed", "", rsItem1, `{"type":"web_search_call","id":"ws_1"}`, fcItem1),
	)
	require.Len(t, resp.ToolCalls, 1, "unknown items are ignored for calls")
}

func TestResponsesParseFinish(t *testing.T) {
	c := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())

	truncated := parseResponses(t, c, responsesBody("incomplete",
		`,"incomplete_details":{"reason":"max_output_tokens"}`, rsItem1, fcItem1))
	assert.Equal(t, llmwire.FinishLength, truncated.FinishType, "a truncated call never reads as tool_calls")
	assert.Equal(t, "incomplete:max_output_tokens", truncated.ProviderFinishReason)

	filtered := parseResponses(t, c, responsesBody("incomplete",
		`,"incomplete_details":{"reason":"content_filter"}`, msgItem1))
	assert.Equal(t, llmwire.FinishUnknown, filtered.FinishType)
	assert.Equal(t, "incomplete:content_filter", filtered.ProviderFinishReason)

	bare := parseResponses(t, c, responsesBody("incomplete", "", msgItem1))
	assert.Equal(t, llmwire.FinishUnknown, bare.FinishType)
	assert.Equal(t, "incomplete", bare.ProviderFinishReason)

	refusal := parseResponses(t, c, responsesBody("completed", "",
		`{"type":"message","id":"msg_r","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}`))
	assert.Equal(t, "no", refusal.Text)
	assert.Equal(t, llmwire.FinishStop, refusal.FinishType)

	_, err := c.parseResponsesBody(zap.NewNop(), responsesBody("failed",
		`,"error":{"code":"server_error","message":"boom"}`), time.Now())
	require.ErrorContains(t, err, "boom")
}

func TestResponsesParseUsage(t *testing.T) {
	model := reasoningModel()
	model.Pricing = &config.ModelPricing{InputPrice: 1, OutputPrice: 10, CacheReadPrice: 0.1}
	c := newTestResponsesClient(t, "https://api.openai.com/v1", model)

	resp := parseResponses(t, c, responsesBody("completed", "", msgItem1))

	require.NotNil(t, resp.Usage)
	assert.Equal(t, 100, resp.Usage.PromptTokens)
	assert.Equal(t, 40, resp.Usage.CacheTokens)
	assert.Equal(t, 10, resp.Usage.CompletionTokens)
	assert.InDelta(t, (60*1+10*10+40*0.1)/1_000_000, resp.CostUSD, 1e-12)
}

// persist turns a parsed response into the assistant row the session stores.
func persist(resp *llmwire.Response) llmwire.Message {
	return llmwire.Message{
		Role: llmwire.RoleAssistant, Content: resp.Text, ToolCalls: resp.ToolCalls, ReasoningRaw: resp.ReasoningRaw,
	}
}

func inputItems(t *testing.T, c *openAIResponsesClient, messages []llmwire.Message) []map[string]any {
	t.Helper()

	var items []map[string]any
	require.NoError(t, roundTripJSON(c.convertInput(messages), &items))

	return items
}

func TestResponsesReplaysReasoningBeforeItsCall(t *testing.T) {
	c := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())
	resp := parseResponses(t, c, responsesBody("completed", "", rsItem1, fcItem1))

	history := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "go"},
		persist(resp),
		{Role: llmwire.RoleTool, ToolCallID: "call_1", ToolName: "read", Content: "out"},
	}
	require.NoError(t, llmtest.ValidateToolPairing(history))

	items := inputItems(t, c, history)
	require.Len(t, items, 4)
	assert.Equal(t, map[string]any{
		"type": "reasoning", "id": "rs_1", "encrypted_content": "ENC1", "summary": []any{},
	}, items[1])
	assert.Equal(t, map[string]any{
		"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read", "arguments": `{"p":1}`,
	}, items[2])
	assert.Equal(t, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "out"}, items[3])
}

func TestResponsesReplaysInterleavedOrder(t *testing.T) {
	c := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())
	resp := parseResponses(t, c, responsesBody("completed", "", rsItem1, fcItem1, rsItem2, fcItem2))

	items := inputItems(t, c, []llmwire.Message{persist(resp)})

	var ids []any
	for _, item := range items {
		ids = append(ids, item["id"])
	}

	assert.Equal(t, []any{"rs_1", "fc_1", "rs_2", "fc_2"}, ids)
}

func TestResponsesReplaysMessageVerbatim(t *testing.T) {
	c := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())
	resp := parseResponses(t, c, responsesBody("completed", "", rsItem1, msgItem1))

	items := inputItems(t, c, []llmwire.Message{persist(resp)})
	require.Len(t, items, 2)
	assert.Equal(t, "rs_1", items[0]["id"])
	assert.Equal(t, "msg_1", items[1]["id"])
}

// Each case breaks exactly one replay guard; empty Content keeps the
// text-without-message guard from masking the others.
func TestResponsesFallsBackToPlainForm(t *testing.T) {
	c := newTestResponsesClient(t, "https://api.openai.com/v1", reasoningModel())
	resp := parseResponses(t, c, responsesBody("completed", "", rsItem1, fcItem1))
	skeleton, _ := unwrapReasoning(resp.ReasoningRaw, "gpt-6-luna")
	call := []llmwire.ToolCall{{ID: "call_1", Name: "read", Arguments: []byte(`{"p":1}`)}}
	sealed := func(entries string) json.RawMessage {
		return wrapReasoning("gpt-6-luna", json.RawMessage(entries))
	}
	ref := `{"kind":"function_call","id":"fc_1","call_id":"call_1"}`
	rs := `{"kind":"reasoning","raw":` + rsItem1 + `}`

	for name, msg := range map[string]llmwire.Message{
		"another model": {ToolCalls: call, ReasoningRaw: wrapReasoning("gpt-6.1-sol", skeleton)},
		"ref without its call": {
			ToolCalls:    []llmwire.ToolCall{{ID: "call_9", Name: "read", Arguments: []byte(`{}`)}},
			ReasoningRaw: resp.ReasoningRaw,
		},
		"duplicate ref":                {ToolCalls: call, ReasoningRaw: sealed(`{"format":"openai-responses/v1","items":[` + rs + `,` + ref + `,` + ref + `]}`)},
		"no format tag":                {ToolCalls: call, ReasoningRaw: sealed(`{"items":[` + rs + `,` + ref + `]}`)},
		"reasoning without follow":     {ToolCalls: call, ReasoningRaw: sealed(`{"format":"openai-responses/v1","items":[` + ref + `,` + rs + `]}`)},
		"unknown entry kind":           {ToolCalls: call, ReasoningRaw: sealed(`{"format":"openai-responses/v1","items":[` + rs + `,` + ref + `,{"kind":"web_search_call"}]}`)},
		"chat completions payload":     {ToolCalls: call, ReasoningRaw: sealed(`[{"type":"reasoning.encrypted","data":"X"}]`)},
		"text without a message entry": {ToolCalls: call, Content: "text", ReasoningRaw: resp.ReasoningRaw},
	} {
		msg.Role = llmwire.RoleAssistant
		items := inputItems(t, c, []llmwire.Message{msg})

		want := len(msg.ToolCalls)
		if msg.Content != "" {
			want++
		}

		require.Len(t, items, want, name)

		if msg.Content != "" {
			assert.Equal(t, map[string]any{"role": "assistant", "content": "text"}, items[0], name)
		}

		for _, item := range items {
			assert.NotEqual(t, "reasoning", item["type"], name)
			assert.NotContains(t, item, "id", name)
		}
	}
}
