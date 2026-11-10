package llm

import "encoding/json"

const (
	respItemReasoning    = "reasoning"
	respItemMessage      = "message"
	respItemFunctionCall = "function_call"
	respItemCallOutput   = "function_call_output"

	respPartInputText  = "input_text"
	respPartInputImage = "input_image"
	respPartOutputText = "output_text"
	respPartRefusal    = "refusal"

	respStatusCompleted  = "completed"
	respStatusIncomplete = "incomplete"
	respStatusFailed     = "failed"

	respIncompleteMaxOutput = "max_output_tokens"
	respIncludeEncrypted    = "reasoning.encrypted_content"

	// responsesSkeletonFormat tags an envelope payload as this wire's, so a
	// Chat Completions payload under the same model id is recognised as foreign.
	responsesSkeletonFormat = "openai-responses/v1"
)

// responsesRequest is the body of POST {base_url}/responses. Store has no
// omitempty: the API stores by default, so false must always be on the wire.
type responsesRequest struct {
	Model           string              `json:"model"`
	Instructions    string              `json:"instructions,omitempty"`
	Input           []any               `json:"input"`
	Tools           []responsesTool     `json:"tools,omitempty"`
	ToolChoice      string              `json:"tool_choice,omitempty"`
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
	Store           bool                `json:"store"`
	Include         []string            `json:"include,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
}

// responsesTool is an internally tagged function tool. Strict is always sent:
// an omitted strict makes the API attempt strict mode, which MCP schemas fail.
type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

type responsesReply struct {
	Status            string               `json:"status"`
	Error             *responsesError      `json:"error"`
	IncompleteDetails *responsesIncomplete `json:"incomplete_details"`
	Output            []json.RawMessage    `json:"output"`
	Usage             responsesUsage       `json:"usage"`
}

type responsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesIncomplete struct {
	Reason string `json:"reason"`
}

type responsesItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	EncryptedContent string          `json:"encrypted_content"`
	Content          []responsesPart `json:"content"`
}

type responsesPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responsesUsage struct {
	InputTokens        int                    `json:"input_tokens"`
	InputTokensDetails *responsesInputDetails `json:"input_tokens_details"`
	OutputTokens       int                    `json:"output_tokens"`
	TotalTokens        int                    `json:"total_tokens"`
}

type responsesInputDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// responsesSkeleton is the envelope payload: the response's output order, with
// reasoning and message items verbatim and function calls as id references.
type responsesSkeleton struct {
	Format string          `json:"format"`
	Items  []skeletonEntry `json:"items"`
}

type skeletonEntry struct {
	Kind   string          `json:"kind"`
	Raw    json.RawMessage `json:"raw,omitempty"`
	ID     string          `json:"id,omitempty"`
	CallID string          `json:"call_id,omitempty"`
}
