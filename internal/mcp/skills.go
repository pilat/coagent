package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

const skillsExtension = "io.modelcontextprotocol/skills"

type Skill struct {
	URI         string
	Name        string
	Frontmatter map[string]any
	Files       []SkillFile
}

type SkillFile struct {
	URI    string
	Digest string
	Size   int64
}

type skillsPage struct {
	mcp.PaginatedResult
	Skills []json.RawMessage `json:"skills"`
}

func (c *Client) Skills() []Skill { return c.skills }

func (c *Client) refreshSkills(ctx context.Context) {
	c.skills = nil

	if !c.skillsDeclared {
		return
	}

	skills, err := c.listSkills(ctx)
	if err != nil {
		logger.Ctx(ctx).Warn("mcp_skills_list_failed", zap.String("name", c.name), zap.Error(err))
		return
	}

	c.skills = skills
}

func (c *Client) listSkills(ctx context.Context) ([]Skill, error) {
	var skills []Skill

	cursor := ""
	seen := make(map[string]bool)
	log := logger.Ctx(ctx).With(zap.String("name", c.name))

	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}

		var page skillsPage

		if err := c.extensionCall(ctx, "skills/list", params, &page); err != nil {
			return nil, err
		}

		if page.ResultType != "" && page.ResultType != mcp.ResultTypeComplete {
			return nil, fmt.Errorf("skills/list: incomplete result %s", page.ResultType)
		}

		for _, raw := range page.Skills {
			skill, reason := decodeSkillEntry(raw)
			if reason != "" {
				log.Warn("mcp_skill_declined", zap.String("uri", skill.URI), zap.String("reason", reason))

				continue
			}

			skills = append(skills, skill)
		}

		cursor = string(page.NextCursor)
		if cursor == "" {
			return skills, nil
		}

		if seen[cursor] {
			return nil, fmt.Errorf("skills/list: repeated cursor %s", cursor)
		}

		seen[cursor] = true
	}
}
