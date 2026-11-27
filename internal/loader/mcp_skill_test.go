package loader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMCPSkill_DecodesListingFrontmatter(t *testing.T) {
	t.Parallel()

	origin := SkillOrigin{Frontmatter: map[string]any{
		"name": "pdf", "description": "Read PDF files", "user-invocable": false, "disable-model-invocation": true,
	}}
	skill, err := NewMCPSkill("docs:pdf", origin)
	require.NoError(t, err)
	assert.Equal(t, "docs:pdf", skill.Name)
	assert.Equal(t, "Read PDF files", skill.Description)
	assert.False(t, skill.IsUserInvocable())
	assert.False(t, skill.IsModelInvocable())
	assert.Equal(t, origin, *skill.Origin)

	origin.Frontmatter["user-invocable"] = "yes"
	_, err = NewMCPSkill("docs:pdf", origin)
	require.Error(t, err)
}

func TestAddMCPSkill_KeepsTheExistingName(t *testing.T) {
	t.Parallel()

	service := New()
	disk := &Skill{Name: "docs:pdf", Content: "disk"}
	service.RegisterSkill(disk)
	assert.False(t, service.AddMCPSkill(&Skill{Name: "docs:pdf", Content: "server"}))
	assert.Same(t, disk, service.GetSkill("docs:pdf"))
	assert.True(t, service.AddMCPSkill(&Skill{Name: "docs:guide"}))
}
