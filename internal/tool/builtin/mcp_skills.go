package builtin

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcp"
)

type MCPSkill struct {
	Server string
	Skill  *loader.Skill
}

func (s *Stack) MCPSkills() []MCPSkill {
	return s.mcpSkills
}

func offerMCPSkills(ctx context.Context, projectID int64, servers []string, groups []mcp.ServerSkills) []MCPSkill {
	dir, err := coagenthome.ProcessProjectDir(projectID)
	owners := mcpLabelOwners(servers)

	var offered []MCPSkill

	for _, group := range groups {
		reason := ""

		switch {
		case err != nil:
			reason = "no project directory"
		case url.PathEscape(group.Server) == "." || url.PathEscape(group.Server) == "..":
			reason = "server name cannot name a directory"
		case owners[mcpServerLabel(group.Server)] != group.Server:
			reason = fmt.Sprintf(
				"label %s is taken by server %s",
				mcpServerLabel(group.Server),
				owners[mcpServerLabel(group.Server)],
			)
		}

		if reason != "" {
			declineMCPSkill(ctx, group.Server, "", reason)
			continue
		}

		serverDir := filepath.Join(dir, coagenthome.MCPSkillsDirName, url.PathEscape(group.Server))
		offered = append(offered, offerServerSkills(ctx, serverDir, group.Server, group.Skills, group.Client)...)
	}

	slices.SortFunc(offered, func(a, b MCPSkill) int {
		if order := cmp.Compare(a.Server, b.Server); order != 0 {
			return order
		}

		return cmp.Compare(a.Skill.Name, b.Skill.Name)
	})

	return offered
}

func mcpLabelOwners(servers []string) map[string]string {
	owners := make(map[string]string)

	slices.Sort(servers)

	for _, server := range servers {
		label := mcpServerLabel(server)
		if _, exists := owners[label]; !exists {
			owners[label] = server
		}
	}

	return owners
}

func offerServerSkills(ctx context.Context, dir, server string, skills []mcp.Skill, reader skillFileReader) []MCPSkill {
	names := mcpSkillNames(mcpServerLabel(server), skills)
	var offered []MCPSkill

	for i, entry := range skills {
		source, err := newMCPSkillFiles(dir, entry, reader)
		if err != nil {
			declineMCPSkill(ctx, server, entry.URI, err.Error())

			continue
		}

		skill, err := loader.NewMCPSkill(names[i], loader.SkillOrigin{
			Server: server, URI: entry.URI, Files: slices.Sorted(maps.Keys(source.files)),
			Frontmatter: entry.Frontmatter, Source: source,
		})
		if err != nil {
			declineMCPSkill(ctx, server, entry.URI, "invalid frontmatter: "+err.Error())

			continue
		}

		offered = append(offered, MCPSkill{Server: server, Skill: skill})
	}

	return offered
}

func mcpSkillNames(label string, skills []mcp.Skill) []string {
	counts := make(map[string]int)
	for _, skill := range skills {
		counts[skill.Name]++
	}

	names := make([]string, len(skills))
	for i, skill := range skills {
		name := skill.Name
		if counts[name] > 1 {
			name = strings.TrimSuffix(skill.URI, "/SKILL.md")
			if _, path, ok := strings.Cut(name, "://"); ok {
				name = path
			}
		}

		names[i] = label + ":" + name
	}

	return names
}

func mcpServerLabel(server string) string {
	label := []byte(server)
	for i, b := range label {
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-' {
			continue
		}

		label[i] = '_'
	}

	return string(label)
}

func declineMCPSkill(ctx context.Context, server, uri, reason string) {
	logger.Ctx(ctx).
		Warn("mcp_skill_declined", zap.String("name", server), zap.String("uri", uri), zap.String("reason", reason))
}
