package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

func TestParseSkillCommand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		message     string
		wantName    string
		wantArgs    string
		wantMatched bool
		wantError   bool
	}{
		{name: "simple", message: "/skill review", wantName: "review", wantMatched: true},
		{name: "leading whitespace", message: " \t/skill review", wantName: "review", wantMatched: true},
		{name: "newline separator", message: "/skill\nreview", wantName: "review", wantMatched: true},
		{
			name:        "multiline arguments",
			message:     "/skill  review  first\n  second  ",
			wantName:    "review",
			wantArgs:    "first\n  second",
			wantMatched: true,
		},
		{name: "missing name", message: "/skill", wantMatched: true, wantError: true},
		{name: "whitespace only name", message: "/skill   ", wantMatched: true, wantError: true},
		{name: "lookalike", message: "/skillful review"},
		{name: "colon command", message: "/skill:review"},
		{name: "later occurrence", message: "please /skill review"},
		{name: "other command", message: "/status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, args, matched, err := parseSkillCommand(tc.message)

			assert.Equal(t, tc.wantName, name)
			assert.Equal(t, tc.wantArgs, args)
			assert.Equal(t, tc.wantMatched, matched)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPrepareUserMessageExpandsDirectSkillInvocation(t *testing.T) {
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{
		Name:                   "review",
		Description:            "Review changes",
		DisableModelInvocation: true,
		Content:                "Review $ARGUMENTS carefully.",
	})

	s := newMockSvc(t, nil, "")
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

	s := newMockSvc(t, nil, "")
	s.loader = ldr
	_, err := s.PrepareUserMessage("/skill hidden")
	require.ErrorContains(t, err, "skill unavailable: hidden")
}

func TestSetupRegistrySkillToolUsesSessionLoader(t *testing.T) {
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "review", Content: "Review changes."})

	incomingRegistry := tool.NewRegistry()
	incomingRegistry.Register(builtin.NewSkillTool(loader.New()))
	agentTypes := registry.NewSet(nil)
	agentConfig, ok := agentTypes.Get(registry.AgentTypeBuild)
	require.True(t, ok)

	s := &svc{
		agentTypes: agentTypes,
		loader:     ldr,
		prompt:     newPromptBuilder("base", "", ""),
	}
	s.setupRegistry(params{Registry: incomingRegistry, Loader: ldr}, agentConfig)

	result, err := s.registry.Execute(
		context.Background(),
		tool.IDSkill,
		json.RawMessage(`{"name":"review"}`),
	)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Review changes.")
}
