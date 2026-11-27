package builtin

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/mcp"
)

func TestSkillToolActivatesMCPSkill(t *testing.T) {
	data := []byte("---\nname: pdf\ndescription: PDF\n---\nProcess $ARGUMENTS\n")
	reader := &skillReaderFake{data: data}
	offered := offerServerSkills(t.Context(), t.TempDir(), "docs", []mcp.Skill{
		{
			Name: "pdf",
			URI:  "skill://pdf/SKILL.md",
			Frontmatter: map[string]any{
				"name":        "pdf",
				"description": "PDF",
			},
			Files: []mcp.SkillFile{listedSkillFile("skill://pdf/SKILL.md", data)},
		},
	}, reader)
	require.Len(t, offered, 1)
	ldr := loader.New()
	ldr.RegisterSkill(offered[0].Skill)
	result, err := NewSkillTool(ldr).Execute(t.Context(), json.RawMessage(`{"name":"docs:pdf","args":"report"}`))
	require.NoError(t, err)
	assert.Contains(t, result.Output, "<name>docs:pdf</name>")
	assert.Contains(t, result.Output, `<origin>MCP server "docs"`)
	assert.Contains(t, result.Output, "Process report")
	assert.Equal(t, []string{"🔧 Activated skill: docs:pdf"}, result.DirectMessages)
	assert.Empty(t, result.Metadata["path"])
}

func TestSkillToolOpensMCPSkillFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		hidden  bool
		content string
	}{
		{name: "text", data: []byte("hello"), content: "hello"},
		{name: "binary", data: []byte{0, 0xff}, content: "Binary content is not shown; use the local path."},
		{name: "hidden", data: []byte("hidden"), hidden: true, content: "hidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			reader := &skillReaderFake{data: tc.data}
			frontmatter := map[string]any{"description": "PDF", "disable-model-invocation": tc.hidden}
			offered := offerServerSkills(t.Context(), dir, "docs", []mcp.Skill{{
				Name: "pdf", URI: "skill://pdf/SKILL.md", Frontmatter: frontmatter,
				Files: []mcp.SkillFile{listedSkillFile("skill://pdf/a&b.txt", tc.data)},
			}}, reader)
			require.Len(t, offered, 1)
			ldr := loader.New()
			ldr.RegisterSkill(offered[0].Skill)
			impl := NewSkillTool(ldr)
			result, err := impl.Execute(t.Context(), json.RawMessage(`{"name":"docs:pdf","file":"a&b.txt"}`))
			require.NoError(t, err)
			path := result.Metadata["path"]
			assert.Equal(t, "docs:pdf a&b.txt", result.Title)
			assert.Equal(t, "docs:pdf", result.Metadata["skill"])
			assert.Equal(t, "a&b.txt", result.Metadata["file"])
			assert.Equal(
				t,
				fmt.Sprintf(
					"Opened file a&b.txt of skill docs:pdf (%d bytes, verified against the server's listing).\nLocal path: %s\n\n%s",
					len(tc.data),
					path,
					tc.content,
				),
				result.Output,
			)
			assert.Empty(t, result.DirectMessages)
			_, err = impl.Execute(t.Context(), json.RawMessage(`{"name":"docs:pdf","file":"a&amp;b.txt"}`))
			require.NoError(t, err)
			assert.Equal(t, 1, reader.calls)
		})
	}
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "disk"})
	for _, tc := range []struct{ params, want string }{
		{params: `{"name":"disk","args":"x","file":"a"}`, want: "args and file cannot be combined"},
		{params: `{"name":"disk","file":"a"}`, want: "skill disk is not served by an MCP server; file applies only to MCP skills"},
		{params: `{"name":"missing","file":"a"}`, want: "skill unavailable: missing"},
	} {
		_, err := NewSkillTool(ldr).Execute(t.Context(), json.RawMessage(tc.params))
		require.EqualError(t, err, tc.want)
	}
	_, err := NewSkillTool(nil).Execute(t.Context(), json.RawMessage(`{"name":"docs:pdf","file":"a"}`))
	require.EqualError(t, err, "skill catalog is unavailable")
}
