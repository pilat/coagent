package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

// The expansion is programmatic: what reaches the provider is the rendered skill
// envelope with its arguments substituted, not the command the human typed.
func TestHarnessScenario_SkillCommandExpandsBeforeTheModelCall(t *testing.T) {
	const skillName = "release-notes"

	h, rec := newSkillHarness(t, map[string]string{
		skillName: skillDoc(skillName, "Draft release notes", "Draft notes for $ARGUMENTS."),
	}, plainRespond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "warm up", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/skill "+skillName+" v1.2.3"))
	h.waitUntil("expanded skill reaches the transcript", func() bool {
		return countMessagesWithSkill(h.parentMessages(sessionID), skillName) == 1
	})
	h.mgr.waitIdle(sessionID)

	calls := rec.snapshot()
	require.NotEmpty(t, calls)

	last := calls[len(calls)-1]
	require.True(t, hasUserContaining(last.msgs, "<skill>\n<name>"+skillName+"</name>"),
		"the model receives the rendered envelope")
	assert.True(t, hasUserContaining(last.msgs, "Draft notes for v1.2.3."), "arguments are substituted")
	assert.False(t, hasUserContaining(last.msgs, "/skill "+skillName), "the raw command never reaches the model")

	h.requireInboxDrained(sessionID)
	assert.Empty(t, warningNotices(collector.snapshot(), sessionID), "a successful invocation warns about nothing")
}

// Model discovery and direct user invocation are independent switches, and each
// one must gate only its own path.
func TestHarnessScenario_SkillInvocationPolicySeparatesUserAndModelPaths(t *testing.T) {
	for _, tc := range []struct {
		name         string
		skill        string
		frontmatter  string
		wantExpanded bool
		wantInPrompt bool
	}{
		{
			name:         "user-invocable false is refused for /skill but stays model-visible",
			skill:        "model-only",
			frontmatter:  "user-invocable: false",
			wantInPrompt: true,
		},
		{
			name:         "disable-model-invocation true hides the skill but keeps /skill working",
			skill:        "user-only",
			frontmatter:  "disable-model-invocation: true",
			wantExpanded: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rec := newSkillHarness(t, map[string]string{
				tc.skill: skillDoc(tc.skill, "policy probe", "Body of "+tc.skill+".", tc.frontmatter),
			}, plainRespond)
			collector := collectEvents(h.mgr.PubSub().SubscribeAll())

			defer func() {
				collector.stop()
				h.shutdown()
			}()

			sessionID, err := h.mgr.Send(h.ctx, h.projectID, "warm up", "fake-model", nil)
			require.NoError(t, err)
			h.mgr.waitIdle(sessionID)

			calls := rec.snapshot()
			require.NotEmpty(t, calls)
			assert.Equal(t, tc.wantInPrompt, strings.Contains(calls[0].system, "**"+tc.skill+"**"),
				"model-invocable skills — and only those — are announced in the system prompt")

			require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/skill "+tc.skill))

			if tc.wantExpanded {
				h.waitUntil("expanded skill reaches the transcript", func() bool {
					return countMessagesWithSkill(h.parentMessages(sessionID), tc.skill) == 1
				})
				h.mgr.waitIdle(sessionID)
				assert.Empty(t, warningNotices(collector.snapshot(), sessionID))

				return
			}

			collector.waitFor(t, "rejection notice", func(e []controllerapi.SessionNotification) bool {
				return len(warningNotices(e, sessionID)) > 0
			})
			h.mgr.waitIdle(sessionID)

			assert.Contains(t, warningNotices(collector.snapshot(), sessionID)[0], "skill unavailable: "+tc.skill)
			assert.Zero(t, countMessagesWithSkill(h.parentMessages(sessionID), tc.skill))
			h.requireInboxDrained(sessionID)
		})
	}
}
