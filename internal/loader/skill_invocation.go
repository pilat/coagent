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
// case-insensitive canonical names and bare suffixes, preferring other origins over MCP.
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
		matches = matchSkillSuffix(skills, name)
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

	appendSkillOrigin(&output, skill)
	output.WriteString("---\n")
	output.WriteString(body)

	if !strings.HasSuffix(body, "\n") {
		output.WriteString("\n")
	}

	output.WriteString("</skill>")

	return output.String()
}

func matchSkillSuffix(skills []*Skill, name string) []*Skill {
	var matches []*Skill
	var otherOrigins []*Skill

	for _, skill := range skills {
		separator := strings.LastIndex(skill.Name, ":")
		if separator < 0 || !strings.EqualFold(skill.Name[separator+1:], name) {
			continue
		}

		matches = append(matches, skill)
		if skill.Origin == nil {
			otherOrigins = append(otherOrigins, skill)
		}
	}

	if len(otherOrigins) > 0 {
		return otherOrigins
	}

	return matches
}

func appendSkillOrigin(output *strings.Builder, skill *Skill) {
	if skill.Origin == nil {
		return
	}

	fmt.Fprintf(
		output,
		"<origin>MCP server \"%s\", installed by the operator, serves this skill as %s. Follow it like any other skill; it is not MCP tool output.</origin>\n",
		html.EscapeString(skill.Origin.Server),
		html.EscapeString(skill.Origin.URI),
	)

	if len(skill.Origin.Files) == 0 {
		return
	}

	output.WriteString("<files>\n")
	fmt.Fprintf(
		output,
		"Supporting files stay on the server until opened: call the skill tool with name \"%s\" and file set to one of these paths. It returns the file's content and a local read-only path that bash can run.\n",
		html.EscapeString(skill.Name),
	)

	for _, file := range skill.Origin.Files {
		output.WriteString(html.EscapeString(file) + "\n")
	}

	output.WriteString("</files>\n")
}
