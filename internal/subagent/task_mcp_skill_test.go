package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/tool"
)

type taskSkillFiles struct{ err error }

func (f *taskSkillFiles) Document(context.Context) ([]byte, error) {
	return []byte("---\nname: pdf\ndescription: Read PDFs\n---\nRead $ARGUMENTS."), f.err
}

func (*taskSkillFiles) Open(context.Context, string) (loader.OpenedFile, error) {
	panic("task must not open supporting files")
}

func TestTaskTool_SeedsFromMCPSkill(t *testing.T) {
	t.Parallel()

	for _, background := range []bool{false, true} {
		source := &taskSkillFiles{}
		skill, err := loader.NewMCPSkill("docs:pdf", loader.SkillOrigin{
			Server: "docs", URI: "skill://pdf/SKILL.md", Source: source,
			Frontmatter: map[string]any{"name": "pdf", "description": "Read PDFs"},
		})
		require.NoError(t, err)
		spawner := &mockSpawner{}
		task := taskToolWithSkills(spawner, skill)
		params, err := json.Marshal(TaskParams{
			Skill:        "docs:pdf",
			SkillArgs:    "the report",
			Description:  "read",
			SubagentType: "general",
			Background:   background,
		})
		require.NoError(t, err)
		_, err = task.Execute(tool.WithCallID(t.Context(), "mcp-seed"), params)
		if background {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, tool.ErrSuspend)
		}
		assert.Contains(t, spawner.lastReq.Prompt, "<origin>MCP server")
		assert.Contains(t, spawner.lastReq.Prompt, "Read the report.")
		assert.Equal(t, 1, spawner.spawnCount)
		source.err = errors.New("SKILL.md: unavailable")
		_, err = task.Execute(tool.WithCallID(t.Context(), "mcp-failure"), params)
		require.ErrorContains(t, err, "render task skill: skill docs:pdf: SKILL.md: unavailable")
		assert.Equal(t, 1, spawner.spawnCount)
	}
}
