package sessionprompt

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/tool"
)

type serverPromptTool struct {
	stubTool
	declaration tool.PromptDeclaration
}

type promptSkillFiles struct{ err error }

func (f *promptSkillFiles) Document(context.Context) ([]byte, error) {
	return []byte("---\nname: pdf\ndescription: Read PDFs\n---\nRead $ARGUMENTS."), f.err
}

func (*promptSkillFiles) Open(context.Context, string) (loader.OpenedFile, error) {
	panic("command must not open supporting files")
}

func TestPrepareUserMessage_ExpandsAndRejectsMCPSkills(t *testing.T) {
	t.Parallel()

	source := &promptSkillFiles{}
	skill, err := loader.NewMCPSkill("docs:pdf", loader.SkillOrigin{
		Server: "docs", URI: "skill://pdf/SKILL.md", Source: source,
		Frontmatter: map[string]any{"name": "pdf", "description": "Read PDFs"},
	})
	require.NoError(t, err)
	ldr := loader.New()
	ldr.RegisterSkill(skill)
	got, err := PrepareUserMessageDetailed(t.Context(), ldr, "/skill docs:pdf the report")
	require.NoError(t, err)
	assert.Equal(t, "docs:pdf", got.SkillName)
	assert.Contains(t, got.Content, "Read the report.")
	assert.Contains(t, got.Content, "<origin>MCP server")
	source.err = errors.New("SKILL.md: unavailable")
	_, err = PrepareUserMessageDetailed(t.Context(), ldr, "/skill docs:pdf")
	require.ErrorContains(t, err, "expand skill command: skill docs:pdf: SKILL.md: unavailable")
}

func (t *serverPromptTool) PromptDeclaration() tool.PromptDeclaration { return t.declaration }

func TestVisibleMCPServers(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.Register(&serverPromptTool{id: "one", declaration: tool.PromptDeclaration{
		MCPServer: "docs", Instructions: "Read docs carefully.",
	}})
	reg.Register(
		&serverPromptTool{id: "two", declaration: tool.PromptDeclaration{MCPServer: "silent"}},
	)
	reg.Register(
		&serverPromptTool{id: "three", declaration: tool.PromptDeclaration{Instructions: "orphan"}},
	)
	assert.Equal(t, map[string]bool{"docs": true, "silent": true}, VisibleMCPServers(reg))
	text := buildToolsSection(reg, false)
	assert.Contains(t, text, "Read docs carefully.")
	assert.NotContains(t, text, "orphan")
	assert.NotContains(t, text, `begin guidance from MCP server "silent"`)
}
