package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func cliRecord(id int64) *sessionstore.SessionRecord {
	return &sessionstore.SessionRecord{ID: id, Attributes: map[string]any{"channel": "cli"}}
}

// The tool exists only where a masked prompt can actually happen: a root session
// that belongs to a terminal.
func TestRegisterSecretTool_Gating(t *testing.T) {
	tests := []struct {
		name string
		rec  *sessionstore.SessionRecord
		want bool
	}{
		{name: "cli root session", rec: cliRecord(1), want: true},
		{
			name: "telegram-shaped root session",
			rec:  &sessionstore.SessionRecord{ID: 2, Attributes: map[string]any{"channel": "telegram"}},
		},
		{name: "session with no channel at all", rec: &sessionstore.SessionRecord{ID: 3}},
		{
			name: "a cli session's subagent",
			rec:  &sessionstore.SessionRecord{ID: 4, ParentID: 1, Attributes: map[string]any{"channel": "cli"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newConfigHarness(t)
			sess := &mockSession{}

			h.mgr.registerSecretTool(context.Background(), tt.rec, sess)
			assert.Equal(t, tt.want, sess.hasTool(tool.IDRequestSecret))
		})
	}
}

// The round trip: the tool suspends and asks, the terminal answers over the
// socket, and the call is resolved with the name — never the value.
func TestRequestSecret_RoundTrip(t *testing.T) {
	t.Cleanup(func() { logger.SetRedactedValues(nil) })

	ctx := context.Background()
	h := newConfigHarness(t)

	sessionID := h.liveSession(t)

	sess := &mockSession{}
	h.mgr.registerSecretTool(ctx, cliRecord(sessionID), sess)

	events := h.mgr.PubSub().SubscribeAll()
	t.Cleanup(func() { h.mgr.PubSub().UnsubscribeAll(events) })

	_, err := sess.registry.Get(tool.IDRequestSecret).Execute(
		tool.WithCallID(ctx, "c1"),
		json.RawMessage(`{"name":"MANAGER_TG_BOT_TOKEN","purpose":"the bot token from BotFather"}`),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)

	assert.True(t, h.mgr.staged.has(sessionID), "the call waits on the person at the keyboard")

	sn := <-events
	require.Equal(t, sessionevent.NotifySecretRequest, sn.Notification.Type)
	assert.Equal(t, "MANAGER_TG_BOT_TOKEN", sn.Notification.SecretName)
	assert.Equal(t, "the bot token from BotFather", sn.Notification.Message)
	require.NotEmpty(t, sn.Notification.RequestID)

	// The value is written by the socket op; only the confirmation comes back here.
	//nolint:gosec // fake credentials
	const secret = "1234:AAH-typed-at-the-terminal"

	_, v := h.mgr.applier.Ops().SetSecret(sn.Notification.SecretName, secret)
	require.True(t, v.Applied)

	require.NoError(t, h.mgr.ResolveSecretRequest(ctx, sn.Notification.RequestID, sn.Notification.SecretName))
	assert.False(t, h.mgr.staged.has(sessionID), "the call is answered")

	// Hygiene: the value is scrubbed from log output, and nothing that travelled
	// through the notification carried it.
	assert.Equal(t, "[REDACTED]", logger.Redact(secret))
	assert.NotContains(t, sn.Notification.Message, secret)
	assert.NotContains(t, sn.Notification.RequestID, secret)
}

func TestRequestSecret_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	sess := &mockSession{}
	h.mgr.registerSecretTool(ctx, cliRecord(testSessionID), sess)
	tl := sess.registry.Get(tool.IDRequestSecret)

	_, err := tl.Execute(tool.WithCallID(ctx, "c1"), json.RawMessage(`{"name":"not a var","purpose":"x"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AN_ENV_VAR")

	_, err = tl.Execute(ctx, json.RawMessage(`{"name":"OK_NAME","purpose":"x"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_call id")
}

// A prompt whose outcome could not be delivered is still open: the terminal that
// asked must be able to try again rather than hold a request id nothing knows.
func TestSecretRequest_UndeliverableOutcomeKeepsThePromptOpen(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	const ghostSession = 4242

	sess := &mockSession{}
	h.mgr.registerSecretTool(ctx, cliRecord(ghostSession), sess)

	_, err := sess.registry.Get(tool.IDRequestSecret).Execute(
		tool.WithCallID(ctx, "c1"),
		json.RawMessage(`{"name":"OPENAI_API_KEY","purpose":"the provider key"}`),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)

	open := h.mgr.PendingSecretRequests(ghostSession)
	require.Len(t, open, 1)

	// The session record does not exist, so delivery cannot land.
	require.Error(t, h.mgr.CancelSecretRequest(ctx, open[0].RequestID))
	assert.Equal(t, open, h.mgr.PendingSecretRequests(ghostSession), "the request survives a failed delivery")
	assert.True(t, h.mgr.staged.has(ghostSession), "and the call is still owed")
}

func TestResolveSecretRequest_UnknownRequest(t *testing.T) {
	h := newConfigHarness(t)

	err := h.mgr.ResolveSecretRequest(context.Background(), "nope", "NAME")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pending secret request")
}

// A subagent of a CLI session must not inherit the prompt, whatever agent type
// it is.
func TestRegisterSecretTool_ChildOfACLISession(t *testing.T) {
	h := newConfigHarness(t)
	child := &mockSession{agentType: registry.AgentTypeExplore}

	h.mgr.registerSecretTool(
		context.Background(),
		&sessionstore.SessionRecord{ID: 9, ParentID: testSessionID, Attributes: map[string]any{"channel": "cli"}},
		child,
	)

	assert.False(t, child.hasTool(tool.IDRequestSecret))
}

// The onboarding guide reaches a terminal chat and nothing else: its script
// tells the model to call request_secret, which no other channel has.
func TestBuiltinSkillsFor(t *testing.T) {
	tests := []struct {
		name string
		rec  *sessionstore.SessionRecord
		want bool
	}{
		{name: "cli root session", rec: cliRecord(1), want: true},
		{
			name: "telegram-shaped root session",
			rec:  &sessionstore.SessionRecord{ID: 2, Attributes: map[string]any{"channel": "telegram"}},
		},
		{
			name: "a cli session's subagent",
			rec:  &sessionstore.SessionRecord{ID: 3, ParentID: 1, Attributes: map[string]any{"channel": "cli"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skills := builtinSkillsFor(context.Background(), tt.rec)

			if !tt.want {
				assert.Empty(t, skills)

				return
			}

			require.Len(t, skills, 1)
			assert.Equal(t, loader.OnboardingSkillName, skills[0].Name)
		})
	}
}
