package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
)

func TestPrepareUserMessageExpandsDirectSkillInvocation(t *testing.T) {
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{
		Name:                   "review",
		Description:            "Review changes",
		DisableModelInvocation: true,
		Content:                "Review $ARGUMENTS carefully.",
	})

	s := newTestAgent()
	s.loader = ldr

	result, err := s.PrepareUserMessage("/skill review current diff")
	require.NoError(t, err)
	assert.Contains(t, result, "<skill>\n<name>review</name>")
	assert.Contains(t, result, "Review current diff carefully.")
}

func TestPrepareUserMessageRejectsNonUserInvocableSkill(t *testing.T) {
	userDisabled := false
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "hidden", UserInvocable: &userDisabled, Content: "hidden"})

	s := newTestAgent()
	s.loader = ldr
	_, err := s.PrepareUserMessage("/skill hidden")
	require.ErrorContains(t, err, "skill unavailable: hidden")
}
