package loader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticSkillCatalog struct {
	skills []*Skill
}

func (c staticSkillCatalog) GetSkill(name string) *Skill {
	for _, skill := range c.skills {
		if skill.Name == name {
			return skill
		}
	}

	return nil
}

func (c staticSkillCatalog) ListModelInvocableSkills() []*Skill {
	var visible []*Skill
	for _, skill := range c.skills {
		if skill.IsModelInvocable() {
			visible = append(visible, skill)
		}
	}

	return visible
}

func TestResolveModelInvocableSkill(t *testing.T) {
	t.Parallel()

	hidden := true
	catalog := staticSkillCatalog{skills: []*Skill{
		{Name: "alpha:review", Content: "alpha"},
		{Name: "beta:deploy", Content: "beta"},
		{Name: "hidden", Content: "hidden", DisableModelInvocation: hidden},
	}}

	for _, name := range []string{" alpha:review ", "ALPHA:REVIEW", "ReViEw"} {
		skill, err := ResolveModelInvocableSkill(catalog, name)
		require.NoError(t, err)
		assert.Equal(t, "alpha:review", skill.Name)
	}

	for _, name := range []string{"hidden", "missing"} {
		_, err := ResolveModelInvocableSkill(catalog, name)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "alpha:review")
		assert.NotContains(t, err.Error(), "beta:deploy hidden")
	}
}

func TestResolveModelInvocableSkillRejectsAmbiguousMatches(t *testing.T) {
	t.Parallel()

	catalog := staticSkillCatalog{skills: []*Skill{
		{Name: "alpha:review", Content: "alpha"},
		{Name: "beta:review", Content: "beta"},
	}}

	_, err := ResolveModelInvocableSkill(catalog, "review")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alpha:review")
	assert.Contains(t, err.Error(), "beta:review")
}
