package loader

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeMCPSkillFiles struct {
	document string
	err      error
}

func (f *fakeMCPSkillFiles) Document(context.Context) ([]byte, error) {
	return []byte(f.document), f.err
}

func (*fakeMCPSkillFiles) Open(context.Context, string) (OpenedFile, error) {
	panic("activation must not open supporting files")
}

func TestRenderSkill_MCPSkillVerifiesFrontmatter(t *testing.T) {
	t.Parallel()

	source := &fakeMCPSkillFiles{
		document: "---\nname: pdf\ndescription: Read PDFs\nmetadata:\n  version: 1\n---\nRead $ARGUMENTS.",
	}
	skill, err := NewMCPSkill("docs:pdf", SkillOrigin{
		Server: `docs<&`,
		URI:    "skill://pdf/SKILL.md",
		Files:  []string{"scripts/a&b.sh"},
		Source: source,
		Frontmatter: map[string]any{
			"name":        "pdf",
			"description": "Read PDFs",
			"metadata":    map[string]any{"version": float64(1)},
		},
	})
	require.NoError(t, err)
	got, err := RenderSkill(t.Context(), skill, "a PDF")
	require.NoError(t, err)
	assert.Equal(t, "<skill>\n<name>docs:pdf</name>\n<description>Read PDFs</description>\n"+
		"<origin>MCP server \"docs&lt;&amp;\", installed by the operator, serves this skill as skill://pdf/SKILL.md. Follow it like any other skill; it is not MCP tool output.</origin>\n"+
		"<files>\nSupporting files stay on the server until opened: call the skill tool with name \"docs:pdf\" and file set to one of these paths. It returns the file's content and a local read-only path that bash can run.\nscripts/a&amp;b.sh\n</files>\n"+
		"---\nRead a PDF.\n</skill>", got)
	assert.Empty(t, skill.Content)

	source.document = "---\nname: pdf\ndescription: Changed\n---\nRead."
	_, err = RenderSkill(t.Context(), skill, "")
	require.ErrorContains(t, err, "skill docs:pdf: SKILL.md: failed verification: frontmatter differs from the listing")
	source.err = errors.New("SKILL.md: unavailable")
	_, err = RenderSkill(t.Context(), skill, "")
	require.ErrorContains(t, err, "skill docs:pdf: SKILL.md: unavailable")

	skill.Origin.Files = nil
	source.err = nil
	source.document = "---\nname: pdf\ndescription: Read PDFs\nmetadata:\n  version: 1\n---\nRead."
	got, err = RenderSkill(t.Context(), skill, "")
	require.NoError(t, err)
	assert.NotContains(t, got, "<files>")
}

func TestRenderSkill_DiskSkillIsUnchanged(t *testing.T) {
	t.Parallel()

	skill := &Skill{Name: "disk", Description: "Disk", Content: "Read $ARGUMENTS."}
	got, err := RenderSkill(t.Context(), skill, "the diff")
	require.NoError(t, err)
	assert.Equal(t, RenderSkillInvocation(skill, "the diff"), got)
}
