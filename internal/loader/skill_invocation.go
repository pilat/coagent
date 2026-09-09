package loader

import (
	"errors"
	"fmt"
	"html"
	"strings"
)

// SkillCatalog is the model-facing subset of the loaded skill registry.
type SkillCatalog interface {
	GetSkill(name string) *Skill
	ListModelInvocableSkills() []*Skill
}

// ResolveModelInvocableSkill resolves exact names first, then unambiguous
// case-insensitive canonical names and bare suffixes.
func ResolveModelInvocableSkill(catalog SkillCatalog, name string) (*Skill, error) {
	if catalog == nil {
		return nil, errors.New("skill catalog is unavailable")
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("skill name is required")
	}

	if skill := catalog.GetSkill(name); skill != nil && skill.IsModelInvocable() {
		return skill, nil
	}

	skills := catalog.ListModelInvocableSkills()
	matches := make([]*Skill, 0, 1)

	for _, skill := range skills {
		if strings.EqualFold(skill.Name, name) {
			matches = append(matches, skill)
		}
	}

	if len(matches) == 1 {
		return matches[0], nil
	}

	if len(matches) == 0 {
		for _, skill := range skills {
			separator := strings.LastIndex(skill.Name, ":")
			if separator >= 0 && strings.EqualFold(skill.Name[separator+1:], name) {
				matches = append(matches, skill)
			}
		}
	}

	if len(matches) == 1 {
		return matches[0], nil
	}

	names := make([]string, len(skills))
	for i, skill := range skills {
		names[i] = skill.Name
	}

	return nil, fmt.Errorf("skill unavailable: %s\nAvailable skills: %v", name, names)
}

// RenderSkillInvocation renders the canonical model-input skill envelope.
func RenderSkillInvocation(skill *Skill, args string) string {
	body := strings.ReplaceAll(skill.Content, "$ARGUMENTS", args)
	if args != "" && !strings.Contains(skill.Content, "$ARGUMENTS") {
		body += "\n\nARGUMENTS: " + args
	}

	var output strings.Builder
	output.WriteString("<skill>\n")
	fmt.Fprintf(&output, "<name>%s</name>\n", html.EscapeString(skill.Name))

	if skill.Description != "" {
		fmt.Fprintf(&output, "<description>%s</description>\n", html.EscapeString(skill.Description))
	}

	output.WriteString("---\n")
	output.WriteString(body)

	if !strings.HasSuffix(body, "\n") {
		output.WriteString("\n")
	}

	output.WriteString("</skill>")

	return output.String()
}
