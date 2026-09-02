package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

type imageStubTool struct {
	id     string
	err    error
	result *tool.Result
}

func (t *imageStubTool) ID() string          { return t.id }
func (t *imageStubTool) ParallelSafe() bool  { return false }
func (t *imageStubTool) Description() string { return "stub" }
func (t *imageStubTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *imageStubTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	if t.err != nil {
		return nil, t.err
	}

	return t.result, nil
}

func newImagePlumbAgent(t *testing.T) (*svc, *imageStubTool) {
	t.Helper()

	_, store, sessionID := newAttachmentsStore(t)
	stub := &imageStubTool{id: "read", result: &tool.Result{
		Title:  "img.png",
		Output: "[img.png]\n<image>...</image>",
		Images: demoRefs,
	}}
	registry := tool.NewRegistry()
	registry.Register(stub)
	s := &svc{
		llmClient:    &compactionMockLLM{},
		ms:           newMessageStore(store, sessionID, nil),
		loopDetector: newLoopDetector(),
		registry:     registry,
		prompt:       newPromptBuilder(testPrompt, "", ""),
	}

	return s, stub
}

// The loop path must carry a tool's Images through recordToolResults onto the
// persisted role-tool row, visible again after reload.
func TestToolImages_PlumbAndPersist(t *testing.T) {
	ctx := context.Background()
	s, _ := newImagePlumbAgent(t)

	calls := []llmwire.ToolCall{{ID: "c1", Name: "read", Arguments: []byte(`{}`)}}
	require.NoError(t, s.ms.addAssistantMessage(
		ctx,
		&llmwire.Response{Text: "", ToolCalls: calls},
	))
	require.NoError(t, executeToolCalls(ctx, s, calls))

	require.NoError(t, s.ms.reloadMessages(ctx))

	msgs := s.ms.getMessages()
	require.Len(t, msgs, 2)
	assert.Equal(t, demoRefs, msgs[1].Images, "refs persist on the role-tool row")
}

// A failed call's error stub replaces everything, including any refs.
func TestToolImages_ErrorStubDropsRefs(t *testing.T) {
	ctx := context.Background()
	s, stub := newImagePlumbAgent(t)
	stub.err = errors.New("boom")

	calls := []llmwire.ToolCall{{ID: "c1", Name: "read", Arguments: []byte(`{}`)}}
	require.NoError(t, s.ms.addAssistantMessage(
		ctx,
		&llmwire.Response{Text: "", ToolCalls: calls},
	))
	require.NoError(t, executeToolCalls(ctx, s, calls))

	msgs := s.ms.getMessages()
	require.Len(t, msgs, 2)
	assert.Contains(t, msgs[1].Content, "Error:")
	assert.Empty(t, msgs[1].Images, "an error stub never claims pixels")
}

// Distinct image reads produce path-bearing success text, so consecutive image
// viewing never fingerprints as a repetitive loop.
func TestToolImages_DistinctReadsDoNotTripLoopDetector(t *testing.T) {
	ctx := context.Background()
	s, stub := newImagePlumbAgent(t)

	calls := []llmwire.ToolCall{{ID: "c", Name: "read", Arguments: []byte(`{}`)}}
	for i := range 5 {
		name := "coagent-view-" + string(rune('a'+i)) + ".png"
		stub.result.Images = []llmwire.ImageRef{{Path: "/tmp/" + name, Mime: llmwire.MimeImagePng, Size: 8}}
		// real read embeds the resolved path in its success text (D6)
		stub.result.Output = "[/tmp/" + name + "]\n<image>...</image>"

		require.NoError(t, executeToolCalls(ctx, s, calls))
	}

	assert.NotEqual(t, actionBlock, s.loopDetector.check())
	assert.Equal(t, 0, s.loopDetector.consecutiveFailureStreak())

	var withRefs int
	for _, m := range s.ms.getMessages() {
		if len(m.Images) > 0 {
			withRefs++
		}
	}

	assert.Equal(t, 5, withRefs)
}
