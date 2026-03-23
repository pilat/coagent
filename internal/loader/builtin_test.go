package loader

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The skill ships inside the binary, so a machine with no config, no
// marketplaces and no project files still has the setup guide.
func TestBuiltinSkill_Onboarding(t *testing.T) {
	skill, err := BuiltinSkill(OnboardingSkillName)
	require.NoError(t, err)

	assert.Equal(t, OnboardingSkillName, skill.Name)
	assert.NotEmpty(t, skill.Description, "the description is what makes the model reach for it")
	assert.True(t, skill.IsUserInvocable())
	assert.False(t, skill.DisableModelInvocation)

	// The parts a first run cannot do without.
	for _, want := range []string{
		"request_secret",
		"@BotFather",
		"@userinfobot",
		"web.telegram.org",
		"Topics",
		"set_manager",
		"restarts the daemon",
	} {
		assert.Contains(t, skill.Content, want, want)
	}

	// The warning has to be there in words, not implied.
	assert.Contains(t, strings.ToLower(skill.Content), "never ask for a credential in the chat")
}

func TestBuiltinSkill_UnknownName(t *testing.T) {
	_, err := BuiltinSkill("no-such-skill")
	require.Error(t, err)
}
