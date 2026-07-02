package builtin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
)

// A skill call that produced no envelope must not hide the calls after it.
func TestExtractRenderedSkillsSkipsEnvelopeLessBatchCalls(t *testing.T) {
	rendered := RenderSkill(&loader.Skill{Name: "invoked", Content: "Instructions."}, "")
	content := "=== skill (call 1) ===\nError: skill unavailable: missing\n\n" +
		"=== skill (call 2) ===\n" + rendered

	skills := ExtractRenderedSkills(content)

	require.Len(t, skills, 1)
	assert.Equal(t, RenderedSkill{Name: "invoked", Envelope: rendered}, skills[0])
}

func TestExtractRenderedSkillsIgnoresNamelessEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "empty name", content: "<skill>\n<name></name>\n---\nBody.\n</skill>"},
		{name: "unterminated name", content: "<skill>\n<name>partial\n---\nBody.\n</skill>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Empty(t, ExtractRenderedSkills(tt.content))
		})
	}
}

func TestExtractRenderedSkillsKeepsPlainContentWhenNoBatchHeaders(t *testing.T) {
	rendered := RenderSkill(&loader.Skill{Name: "solo", Content: "Body."}, "")

	skills := ExtractRenderedSkills("[title]\n" + rendered)

	require.Len(t, skills, 1)
	assert.Equal(t, rendered, skills[0].Envelope)
}

// A skill envelope that opens before the first batch header belongs to the whole
// content, not to a call segment.
func TestExtractRenderedSkillsIgnoresHeadersAfterTheEnvelope(t *testing.T) {
	rendered := RenderSkill(&loader.Skill{Name: "solo", Content: "Body."}, "")
	content := rendered + "\n\n=== read (call 1) ===\nfile contents"

	skills := ExtractRenderedSkills(content)

	require.Len(t, skills, 1)
	assert.Equal(t, "solo", skills[0].Name)
	assert.Equal(t, rendered, skills[0].Envelope)
}

func TestRenderSkillAlwaysTerminatesBodyWithNewline(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "body without trailing newline", content: "Body."},
		{name: "body with trailing newline", content: "Body.\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered := RenderSkill(&loader.Skill{Name: "s", Content: tt.content}, "")

			assert.Equal(t, "<skill>\n<name>s</name>\n---\nBody.\n</skill>", rendered)
			assert.True(t, strings.HasSuffix(rendered, "\n</skill>"))
		})
	}
}

// A malformed envelope in one batch segment must not swallow the segments after it.
func TestExtractRenderedSkillsSkipsMalformedEnvelopes(t *testing.T) {
	rendered := RenderSkill(&loader.Skill{Name: "invoked", Content: "Instructions."}, "")

	tests := []struct {
		name    string
		broken  string
		wantOne bool
	}{
		{name: "unterminated name", broken: "<skill>\n<name>partial\n---\nBody.\n</skill>"},
		{name: "empty name", broken: "<skill>\n<name></name>\n---\nBody.\n</skill>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := "=== skill (call 1) ===\n" + tt.broken + "\n\n=== skill (call 2) ===\n" + rendered

			skills := ExtractRenderedSkills(content)

			require.Len(t, skills, 1)
			assert.Equal(t, "invoked", skills[0].Name)
		})
	}
}
