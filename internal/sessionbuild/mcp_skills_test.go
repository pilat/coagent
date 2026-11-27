package sessionbuild

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

type serverSkillTool struct {
	testTool
	server string
}

func (t serverSkillTool) PromptDeclaration() tool.PromptDeclaration {
	return tool.PromptDeclaration{MCPServer: t.server}
}

func TestRegisterMCPSkills_FollowsToolVisibility(t *testing.T) {
	t.Parallel()

	unfiltered, filtered := tool.NewRegistry(), tool.NewRegistry()
	seen := serverSkillTool{id: "seen-tool", server: "seen"}
	unfiltered.Register(seen)
	unfiltered.Register(serverSkillTool{id: "filtered-tool", server: "filtered"})
	filtered.Register(seen)
	offered := []builtin.MCPSkill{
		{Server: "seen", Skill: &loader.Skill{Name: "seen:pdf"}},
		{Server: "filtered", Skill: &loader.Skill{Name: "filtered:pdf"}},
		{Server: "skillsonly", Skill: &loader.Skill{Name: "skillsonly:pdf"}},
	}
	ldr := loader.New()
	registerMCPSkills(t.Context(), ldr, offered, unfiltered, filtered)
	assert.Empty(t, ldr.ListSkills())
	filtered.Register(builtin.NewSkillTool(ldr))
	registerMCPSkills(t.Context(), ldr, offered, unfiltered, filtered)
	assert.NotNil(t, ldr.GetSkill("seen:pdf"))
	assert.NotNil(t, ldr.GetSkill("skillsonly:pdf"))
	assert.Nil(t, ldr.GetSkill("filtered:pdf"))
	disk := &loader.Skill{Name: "seen:pdf", Content: "disk"}
	ldr.RegisterSkill(disk)
	registerMCPSkills(t.Context(), ldr, offered, unfiltered, filtered)
	assert.Same(t, disk, ldr.GetSkill("seen:pdf"))
}
