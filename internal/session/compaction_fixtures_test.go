package session

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
)

// roundTokens builds an assistant-with-tool-call plus its result, sized in tokens.
func roundTokens(id string, callTokens, resultTokens int) []llmwire.Message {
	return []llmwire.Message{
		{
			Role:      llmwire.RoleAssistant,
			Content:   strings.Repeat("a", callTokens*4),
			ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read"}},
		},
		{Role: llmwire.RoleTool, Content: strings.Repeat("t", resultTokens*4), ToolCallID: id, ToolName: "read"},
	}
}

// validSummary is the scripted summarizer's canonical answer. The runtime never
// requires headings — this shape simply exercises a plausible completed text.
const validSummary = "Checkpoint: goal was to fix the auth bug; login.go edited; tests pass; next: run e2e."

func compactionUserMessage(content string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleUser, Content: content}
}

func compactionAssistantCall(id, content string) llmwire.Message {
	return llmwire.Message{
		Role:      llmwire.RoleAssistant,
		Content:   content,
		ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read"}},
	}
}

func compactionToolResult(id, content string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleTool, Content: content, ToolCallID: id, ToolName: "read"}
}

// contextEventRunner wires a loopRunner around a bare svc so tests can drive
// applyContextEvents without a full runLoop.
func contextEventRunner(s *svc, notes *[]string) *loopRunner {
	return &loopRunner{
		agent:  s,
		log:    zap.NewNop(),
		result: &loopResult{},
		opts: loopOptions{Notify: func(_ context.Context, message string) error {
			*notes = append(*notes, message)

			return nil
		}},
	}
}

// oversizedTranscript builds header + enough large rounds to cross the trigger.
func oversizedTranscript(window int) []llmwire.Message {
	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
	}

	for estimateTokens(messages) < compactionCutoff(window)+10000 {
		messages = append(messages, roundTokens(fmt.Sprintf("big-%d", len(messages)), 20, 900)...)
	}

	return messages
}

func notesContain(notes []string, needle string) bool {
	return countNotes(notes, needle) > 0
}

func countNotes(notes []string, needle string) int {
	count := 0

	for _, n := range notes {
		if strings.Contains(n, needle) {
			count++
		}
	}

	return count
}
