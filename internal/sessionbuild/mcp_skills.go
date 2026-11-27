package sessionbuild

import (
	"context"
	"slices"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

func registerMCPSkills(
	ctx context.Context,
	ldr *loader.Service,
	skills []builtin.MCPSkill,
	unfiltered, reg *tool.Registry,
) {
	if !hasSkillLoader(reg) {
		return
	}

	visible := sessionprompt.VisibleMCPServers(reg)
	registered := sessionprompt.VisibleMCPServers(unfiltered)
	log := logger.Ctx(ctx).Named("sessionbuild.mcp_skills")

	for _, offered := range skills {
		if !visible[offered.Server] && registered[offered.Server] {
			continue
		}

		if !ldr.AddMCPSkill(offered.Skill) {
			log.Warn("mcp_skill_name_taken",
				zap.String("name", offered.Server), zap.String("skill_name", offered.Skill.Name))
		}
	}
}

func hasSkillLoader(reg *tool.Registry) bool {
	return slices.ContainsFunc(reg.List(), func(candidate tool.Tool) bool {
		_, ok := candidate.(tool.SkillLoader)
		return ok
	})
}
