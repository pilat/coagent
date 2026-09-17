package loader

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The skill ships inside the binary, so a machine with no config, no
// marketplaces and no project files still has the management instruction.
func TestBuiltinSkill_Management(t *testing.T) {
	skill, err := BuiltinSkill(ManagementSkillName)
	require.NoError(t, err)

	assert.Equal(t, ManagementSkillName, skill.Name)
	assert.NotEmpty(t, skill.Description)
	assert.True(t, skill.IsUserInvocable())
	assert.True(t, skill.DisableModelInvocation, "the automatically active skill must not be offered again")

	// The parts a service-topic conversation cannot do without.
	for _, want := range []string{
		"single-operator daemon",
		"Do not call the `management` skill",
		"/config",
		"replaces the complete application configuration",
		"restarts the daemon",
		"maintains by hand",
		"Never ask for a credential",
		"rotate",
		"`coagent status`",
		"/status",
		"/new",
		"/spawn",
		"/kill",
		"never\nthis management root",
		"/clear",
		"same service topic",
		"/stop",
		"/model",
		"/schedules",
		"/budget",
		"/compact",
		"/shieldsup",
		"/shieldsdown",
		"grants nothing",
	} {
		assert.Contains(t, skill.Content, want, want)
	}

	// The removed onboarding vocabulary must stay out.
	lowered := strings.ToLower(skill.Content)
	for _, banned := range []string{
		"request_secret",
		"set_provider",
		"terminal",
		"first-run",
		"wizard",
		"sys:coagent",
		"botfather",
	} {
		assert.NotContains(t, lowered, banned, banned)
	}
}

func TestBuiltinSkill_UnknownName(t *testing.T) {
	_, err := BuiltinSkill("no-such-skill")
	require.Error(t, err)
}
