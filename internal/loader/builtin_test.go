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
	assert.NotEmpty(t, skill.Description)
	assert.True(t, skill.IsUserInvocable())
	assert.True(t, skill.DisableModelInvocation, "the automatically active skill must not be offered again")

	// The parts a first run cannot do without.
	for _, want := range []string{
		"request_secret",
		"exactly one config-tool call at a time",
		"deterministic first-run bootstrap",
		"Status does not test provider",
		"a guard\n   refusal returns without restarting",
		"If the user declines",
		"`coagent status`",
		"/status",
		"google-sa",
		"sa_file",
		"provider **name**",
		"Do not add a model merely because",
		"shows only their count and the default",
		"@BotFather",
		"@userinfobot",
		"web.telegram.org",
		"Topics",
		"set_manager",
		"restarts the daemon",
		"reports a rollback",
		"If startup failed",
		"Do not call the\n`onboarding` skill",
		"preserves every omitted field",
		"no-op patch",
		"restarts the daemon without a config",
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
