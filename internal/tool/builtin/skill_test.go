package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/tool"
)

func TestSkillToolUsesModelVisibility(t *testing.T) {
	userDisabled := false
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "both", Content: "both content"})
	ldr.RegisterSkill(&loader.Skill{Name: "model-only", UserInvocable: &userDisabled, Content: "model content"})
	ldr.RegisterSkill(&loader.Skill{Name: "user-only", DisableModelInvocation: true, Content: "user content"})

	skillTool := NewSkillTool(ldr)
	description := skillTool.Description()

	assert.Contains(t, description, "both")
	assert.Contains(t, description, "model-only")
	assert.NotContains(t, description, "user-only")

	result, err := skillTool.Execute(context.Background(), json.RawMessage(`{"name":"model-only"}`))
	require.NoError(t, err)
	assert.Contains(t, result.Output, "model content")

	_, err = skillTool.Execute(context.Background(), json.RawMessage(`{"name":"user-only"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skill unavailable: user-only")
	assert.NotContains(t, err.Error(), "user-only]")
	assert.Contains(t, err.Error(), "[both model-only]")
}

func TestSkillToolDescriptionCapsUnicodeDescriptions(t *testing.T) {
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{
		Name:        "long",
		Description: strings.Repeat("界", 1537),
	})

	description := NewSkillTool(ldr).Description()
	listed := strings.TrimSuffix(
		strings.TrimPrefix(description, "Invokes a loaded skill.\n\nAvailable skills:\n- long: "),
		"\n",
	)

	assert.Equal(t, 1536, utf8.RuneCountInString(listed))
	assert.True(t, utf8.ValidString(listed))
}

func TestRenderSkillArguments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		skill   *loader.Skill
		args    string
		want    string
		notWant string
	}{
		{
			name:  "replaces_every_placeholder",
			skill: &loader.Skill{Name: "fix", Content: "Fix $ARGUMENTS, then verify $ARGUMENTS."},
			args:  "issue 42",
			want:  "Fix issue 42, then verify issue 42.",
		},
		{
			name:    "empty_arguments_replace_placeholder",
			skill:   &loader.Skill{Name: "fix", Content: "Fix $ARGUMENTS now."},
			want:    "Fix  now.",
			notWant: "ARGUMENTS:",
		},
		{
			name:    "appends_arguments_when_placeholder_absent",
			skill:   &loader.Skill{Name: "fix", Content: "Fix the issue."},
			args:    "first\n  second",
			want:    "Fix the issue.\n\nARGUMENTS: first\n  second",
			notWant: "**Arguments**",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := RenderSkill(tc.skill, tc.args)

			assert.Contains(t, result, tc.want)
			if tc.notWant != "" {
				assert.NotContains(t, result, tc.notWant)
			}
		})
	}
}

func TestRenderSkillCanonicalEnvelope(t *testing.T) {
	sk := &loader.Skill{
		Name:        `review<&>`,
		Description: `Check <all> & report`,
		Content:     "Review changes.",
	}

	result := RenderSkill(sk, "")

	assert.Equal(t, `<skill>
<name>review&lt;&amp;&gt;</name>
<description>Check &lt;all&gt; &amp; report</description>
---
Review changes.
</skill>`, result)
}

func TestExtractRenderedSkillIgnoresTransportPrefixes(t *testing.T) {
	rendered := RenderSkill(&loader.Skill{Name: "pilat:review", Content: "Review changes."}, "")

	name, envelope, ok := ExtractRenderedSkill("[title]\n[warning]\n" + rendered)

	require.True(t, ok)
	assert.Equal(t, "pilat:review", name)
	assert.Equal(t, rendered, envelope)

	name, envelope, ok = ExtractRenderedSkill("[timestamp] " + strings.TrimSuffix(rendered, "</skill>"))
	require.True(t, ok)
	assert.Equal(t, "pilat:review", name)
	assert.Equal(t, strings.TrimSuffix(rendered, "</skill>"), envelope)

	_, _, ok = ExtractRenderedSkill("# Skill: pilat:review")
	assert.False(t, ok)
}

func TestExtractRenderedSkillsFromBatchOutput(t *testing.T) {
	first := RenderSkill(&loader.Skill{Name: "first", Content: "First instructions."}, "")
	second := RenderSkill(&loader.Skill{Name: "second", Content: "Second instructions."}, "")
	content := "=== skill (call 1) ===\n[first]\n" + first +
		"\n\n=== skill (call 2) ===\n[second]\n" + second

	skills := ExtractRenderedSkills(content)

	require.Len(t, skills, 2)
	assert.Equal(t, RenderedSkill{Name: "first", Envelope: first}, skills[0])
	assert.Equal(t, RenderedSkill{Name: "second", Envelope: second}, skills[1])
}

func TestExtractRenderedSkillsIgnoresMarkupFromNonSkillBatchCalls(t *testing.T) {
	readMarkup := RenderSkill(&loader.Skill{Name: "not-invoked", Content: "Example."}, "")
	invoked := RenderSkill(&loader.Skill{Name: "invoked", Content: "Instructions."}, "")
	content := "=== read (call 1) ===\n[file]\n" + readMarkup +
		"\n\n=== skill (call 2) ===\n[invoked]\n" + invoked

	skills := ExtractRenderedSkills(content)

	require.Len(t, skills, 1)
	assert.Equal(t, RenderedSkill{Name: "invoked", Envelope: invoked}, skills[0])
}

func TestExtractRenderedSkillsPreservesMarkerInsideBody(t *testing.T) {
	rendered := RenderSkill(&loader.Skill{
		Name: "document-format",
		Content: `Use this example:
<skill>
<name>example</name>
---
Example body.
</skill>`,
	}, "")

	skills := ExtractRenderedSkills(rendered)

	require.Len(t, skills, 1)
	assert.Equal(t, "document-format", skills[0].Name)
	assert.Equal(t, rendered, skills[0].Envelope)
}

func TestExtractRenderedSkillsUsesOuterClosingMarker(t *testing.T) {
	rendered := RenderSkill(&loader.Skill{
		Name: "document-format",
		Content: `The closing marker is </skill>.
Example:
<skill>
<name>demo</name>
---
Example body.
</skill>`,
	}, "")

	skills := ExtractRenderedSkills(rendered)

	require.Len(t, skills, 1)
	assert.Equal(t, "document-format", skills[0].Name)
	assert.Equal(t, rendered, skills[0].Envelope)
}

func TestBatchRejectsSkillInvocation(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(NewSkillTool(loader.New()))
	batch := NewBatchTool(registry)

	_, err := batch.Execute(context.Background(), json.RawMessage(`{
		"calls": [{"tool": "skill", "params": {"name": "review"}}]
	}`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "skill must be invoked directly")
}
