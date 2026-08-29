package session

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/llmwire"
)

// The canonical compaction projection is deterministic JSON Lines: one
// fixed-order object per projected message. Content and tool-call arguments are
// length-safe {encoding,data} envelopes so invalid UTF-8 and malformed JSON stay
// reversible; the summarizer never parses them.

const (
	canonicalEncodingUTF8   = "utf8"
	canonicalEncodingBase64 = "base64"
)

type encodedText struct {
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
}

func encodeText(data []byte) encodedText {
	if utf8.Valid(data) {
		return encodedText{Encoding: canonicalEncodingUTF8, Data: string(data)}
	}

	return encodedText{Encoding: canonicalEncodingBase64, Data: base64Std(data)}
}

type canonicalToolCall struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Arguments encodedText `json:"arguments"`
}

type canonicalAttachment struct {
	Path string `json:"path"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
}

type canonicalRecord struct {
	Role        string                `json:"role"`
	Content     encodedText           `json:"content"`
	ToolCallID  string                `json:"tool_call_id"`
	ToolName    string                `json:"tool_name"`
	ToolCalls   []canonicalToolCall   `json:"tool_calls"`
	Attachments []canonicalAttachment `json:"attachments"`
}

func base64Std(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func canonicalRecordOf(msg llmwire.Message) canonicalRecord {
	record := canonicalRecord{
		Role:        msg.Role,
		Content:     encodeText([]byte(msg.Content)),
		ToolCallID:  msg.ToolCallID,
		ToolName:    msg.ToolName,
		ToolCalls:   []canonicalToolCall{},
		Attachments: []canonicalAttachment{},
	}

	for _, tc := range msg.ToolCalls {
		record.ToolCalls = append(record.ToolCalls, canonicalToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: encodeText(tc.Arguments),
		})
	}

	for _, ref := range msg.Images {
		record.Attachments = append(record.Attachments, canonicalAttachment{
			Path: ref.Path, Mime: ref.Mime, Size: ref.Size,
		})
	}

	return record
}

// serializeCanonical renders messages as deterministic JSONL: one record per
// line, fixed field order, HTML escaping disabled, trailing newline after every
// record. Marshal of plain strings cannot fail, but a session goroutine must
// never panic, so the impossible error path is still carried.
func serializeCanonical(messages []llmwire.Message) (string, error) {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	for _, msg := range messages {
		if err := enc.Encode(canonicalRecordOf(msg)); err != nil {
			return "", fmt.Errorf("canonical serialization: %w", err)
		}
	}

	return buf.String(), nil
}
