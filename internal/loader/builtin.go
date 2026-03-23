package loader

import (
	"bytes"
	"embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

// builtinSkills describe coagent itself, so they ship in the binary — a machine
// with no marketplace and no project files still has them.
//
//go:embed builtin/*/SKILL.md
var builtinSkills embed.FS

// OnboardingSkillName is the setup guide, registered only where its tools exist.
const OnboardingSkillName = "onboarding"

// BuiltinSkill parses one embedded skill by name.
func BuiltinSkill(name string) (*Skill, error) {
	data, err := builtinSkills.ReadFile("builtin/" + name + "/SKILL.md")
	if err != nil {
		return nil, fmt.Errorf("read builtin skill %q: %w", name, err)
	}

	label := "builtin:" + name

	frontmatter, content, err := parseFrontmatter(bytes.NewReader(data), label)
	if err != nil {
		return nil, err
	}

	skill := &Skill{Path: label, Content: content}
	if err := yaml.Unmarshal(frontmatter, skill); err != nil {
		return nil, fmt.Errorf("invalid frontmatter in %s: %w", label, err)
	}

	return skill, nil
}
