package builtin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcp"
)

type skillReaderFake struct {
	data  []byte
	err   error
	calls int
}

func (r *skillReaderFake) ReadSkillFile(context.Context, mcp.SkillFile) ([]byte, error) {
	r.calls++
	return r.data, r.err
}

func TestMCPServerLabel(t *testing.T) {
	t.Parallel()
	for server, want := range map[string]string{"my docs": "my_docs", "a.b/c": "a_b_c", "ok-1_x": "ok-1_x"} {
		assert.Equal(t, want, mcpServerLabel(server))
	}
}

func TestMCPSkillNames_DisambiguatesCollidingNames(t *testing.T) {
	t.Parallel()
	skills := []mcp.Skill{
		{Name: "refunds", URI: "github://acme/billing/refunds/SKILL.md"},
		{Name: "refunds", URI: "github://acme/support/refunds/SKILL.md"},
		{Name: "pdf", URI: "skill://pdf/SKILL.md"},
	}
	assert.Equal(
		t,
		[]string{"docs:acme/billing/refunds", "docs:acme/support/refunds", "docs:pdf"},
		mcpSkillNames("docs", skills),
	)
}

func TestOfferServerSkills_BuildsCatalogEntriesWithoutFetching(t *testing.T) {
	t.Parallel()
	reader := &skillReaderFake{}
	skills := []mcp.Skill{
		{
			Name:        "pdf",
			URI:         "skill://pdf/SKILL.md",
			Frontmatter: map[string]any{"name": "pdf", "description": "PDF tools"},
			Files: []mcp.SkillFile{
				{URI: "skill://pdf/z.txt"},
				{URI: "skill://pdf/SKILL.md"},
				{URI: "skill://pdf/a.txt"},
			},
		},
	}
	offered := offerServerSkills(t.Context(), t.TempDir(), "docs", skills, reader)
	require.Len(t, offered, 1)
	sk := offered[0].Skill
	assert.Equal(t, "docs:pdf", sk.Name)
	assert.Equal(t, "PDF tools", sk.Description)
	assert.Equal(t, "docs", sk.Origin.Server)
	assert.Equal(t, skills[0].URI, sk.Origin.URI)
	assert.Equal(t, skills[0].Frontmatter, sk.Origin.Frontmatter)
	assert.Equal(t, []string{"a.txt", "z.txt"}, sk.Origin.Files)
	assert.Zero(t, reader.calls)
}

func TestOfferServerSkills_DeclinesSkillsWithUnstorableURIs(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zap.WarnLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))
	reader := &skillReaderFake{}
	skills := []mcp.Skill{
		{
			Name: "pdf", URI: "skill://pdf/SKILL.md",
			Frontmatter: map[string]any{"name": "pdf", "description": "PDF tools"},
			Files:       []mcp.SkillFile{{URI: "skill://pdf/SKILL.md"}},
		},
		{
			Name: "ported", URI: "https://host:8443/skills/ported/SKILL.md",
			Frontmatter: map[string]any{"name": "ported", "description": "Ported URI"},
			Files:       []mcp.SkillFile{{URI: "https://host:8443/skills/ported/SKILL.md"}},
		},
		{
			Name: "encoded", URI: "skill://encoded/SKILL.md",
			Frontmatter: map[string]any{"name": "encoded", "description": "Encoded slash"},
			Files:       []mcp.SkillFile{{URI: "skill://encoded/SKILL.md"}, {URI: "skill://encoded/a%2Fb.txt"}},
		},
	}
	offered := offerServerSkills(ctx, t.TempDir(), "docs", skills, reader)
	require.Len(t, offered, 1)
	assert.Equal(t, "docs:pdf", offered[0].Skill.Name)
	assert.Zero(t, reader.calls)
	warnings := logs.FilterMessage("mcp_skill_declined").All()
	require.Len(t, warnings, 2)
	for i, uri := range []string{"https://host:8443/skills/ported/SKILL.md", "skill://encoded/a%2Fb.txt"} {
		fields := warnings[i].ContextMap()
		assert.Equal(t, "docs", fields["name"])
		assert.Equal(t, skills[i+1].URI, fields["uri"])
		assert.Equal(t, "unsafe skill file URI "+uri, fields["reason"])
	}
}

func TestOfferMCPSkills_SkipsServersWhoseLabelIsTaken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	core, logs := observer.New(zap.WarnLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))
	sk := mcp.Skill{Name: "pdf", URI: "skill://pdf/SKILL.md", Frontmatter: map[string]any{"description": "PDF"}}
	groups := []mcp.ServerSkills{
		{Server: "my docs", Skills: []mcp.Skill{sk}},
		{Server: "my_docs", Skills: []mcp.Skill{sk}},
	}
	offered := offerMCPSkills(ctx, 1, []string{"my docs", "my_docs"}, groups)
	require.Len(t, offered, 1)
	assert.Equal(t, "my docs", offered[0].Server)
	entries := logs.FilterMessage("mcp_skill_declined").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "label my_docs is taken by server my docs", entries[0].ContextMap()["reason"])
	assert.Empty(t, offerMCPSkills(t.Context(), 1, []string{"my docs", "my_docs"}, groups[1:]))
}
