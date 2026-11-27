package loader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// SkillFiles reads files verified against an MCP skill's listing.
type SkillFiles interface {
	Document(ctx context.Context) ([]byte, error)
	Open(ctx context.Context, file string) (OpenedFile, error)
}

// OpenedFile contains verified bytes and their local read-only path.
type OpenedFile struct {
	Path string
	Data []byte
}

// SkillOrigin pins an MCP skill to its server and this activation's listing.
type SkillOrigin struct {
	Server      string
	URI         string
	Files       []string
	Frontmatter map[string]any
	Source      SkillFiles
}

// NewMCPSkill builds a catalog entry without fetching its document.
func NewMCPSkill(name string, origin SkillOrigin) (*Skill, error) {
	for _, field := range []string{"user-invocable", "disable-model-invocation"} {
		if value := origin.Frontmatter[field]; value != nil {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("frontmatter %s must be a boolean", field)
			}
		}
	}

	raw, err := json.Marshal(origin.Frontmatter)
	if err != nil {
		return nil, fmt.Errorf("encode frontmatter: %w", err)
	}

	var skill Skill
	if err := yaml.Unmarshal(raw, &skill); err != nil {
		return nil, fmt.Errorf("decode frontmatter: %w", err)
	}

	skill.Name = name
	skill.Origin = &origin

	return &skill, nil
}

// AddMCPSkill keeps the first registration of each canonical name.
func (s *Service) AddMCPSkill(skill *Skill) bool {
	if s.skills[skill.Name] != nil {
		return false
	}

	s.skills[skill.Name] = skill

	return true
}

// RenderSkill verifies an MCP document before rendering the canonical envelope.
func RenderSkill(ctx context.Context, skill *Skill, args string) (string, error) {
	if skill.Origin == nil {
		return RenderSkillInvocation(skill, args), nil
	}

	raw, err := skill.Origin.Source.Document(ctx)
	if err != nil {
		return "", fmt.Errorf("skill %s: %w", skill.Name, err)
	}

	frontmatter, content, err := parseFrontmatter(bytes.NewReader(raw), skill.Origin.URI)
	if err != nil {
		return "", fmt.Errorf("skill %s: %w", skill.Name, err)
	}

	if !matchesSkillFrontmatter(frontmatter, skill.Origin.Frontmatter) {
		return "", fmt.Errorf("skill %s: %w", skill.Name,
			errors.New("SKILL.md: failed verification: frontmatter differs from the listing"))
	}

	loaded := *skill
	loaded.Content = content

	return RenderSkillInvocation(&loaded, args), nil
}

func matchesSkillFrontmatter(raw []byte, listing map[string]any) bool {
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return false
	}

	documentJSON, documentOK := normalizedJSON(document)
	listingJSON, listingOK := normalizedJSON(listing)

	return documentOK && listingOK && reflect.DeepEqual(documentJSON, listingJSON)
}

func normalizedJSON(value any) (any, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}

	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, false
	}

	return normalized, true
}
