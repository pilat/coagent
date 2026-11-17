package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

type hostCall struct {
	caller, target            int64
	filter, message           string
	project, worktree, prompt string
}

type fakeHost struct {
	entries []Entry
	total   int
	err     error
	calls   []hostCall
}

func (h *fakeHost) ListSessions(_ context.Context, caller int64, filter string) ([]Entry, int, error) {
	h.calls = append(h.calls, hostCall{caller: caller, filter: filter})
	return h.entries, h.total, h.err
}

func (h *fakeHost) SendToSession(_ context.Context, caller, target int64, message string) (int64, error) {
	h.calls = append(h.calls, hostCall{caller: caller, target: target, message: message})
	return 25, h.err
}

func (h *fakeHost) SpawnSession(
	_ context.Context,
	caller int64,
	project, worktree, prompt string,
) (int64, string, error) {
	h.calls = append(h.calls, hostCall{caller: caller, project: project, worktree: worktree, prompt: prompt})
	return 26, "api — login", h.err
}

func TestToolsValidateParameters(t *testing.T) {
	for _, tc := range []struct {
		id, args, want string
	}{
		{IDListSessions, `{`, "invalid parameters"},
		{IDListSessions, `{"filter":1}`, "invalid parameters"},
		{IDSendToSession, `{}`, "session_id is required"},
		{IDSendToSession, `{"session_id":-1,"message":"hi"}`, "session_id is required"},
		{IDSendToSession, `{"session_id":1.5,"message":"hi"}`, "invalid parameters"},
		{IDSendToSession, `{"session_id":"1","message":"hi"}`, "invalid parameters"},
		{IDSendToSession, `{"session_id":1,"message":true}`, "invalid parameters"},
		{IDSendToSession, `{"session_id":1}`, "message is required"},
		{IDSendToSession, `{"session_id":1,"message":" \t "}`, "message is required"},
		{IDSpawnSession, `{}`, "project is required"},
		{IDSpawnSession, `{"project":"  ","prompt":"hi"}`, "project is required"},
		{IDSpawnSession, `{"project":"api"}`, "prompt is required"},
		{IDSpawnSession, `{"project":"api","prompt":"\n "}`, "prompt is required"},
		{IDSpawnSession, `{"project":1,"prompt":"hi"}`, "invalid parameters"},
		{IDSpawnSession, `{"project":"api","worktree":1,"prompt":"hi"}`, "invalid parameters"},
	} {
		t.Run(tc.id+tc.args, func(t *testing.T) {
			host := &fakeHost{}
			result, err := findTool(t, host, tc.id).Execute(t.Context(), json.RawMessage(tc.args))
			require.ErrorContains(t, err, tc.want)
			assert.Nil(t, result)
			assert.Empty(t, host.calls)
		})
	}
}

func TestToolsResultsAndHostArguments(t *testing.T) {
	for _, tc := range []struct {
		id, args, title, output string
		call                    hostCall
	}{
		{IDListSessions, `{"filter":"api"}`, "", `No sessions match "api".`, hostCall{caller: 12, filter: "api"}},
		{
			IDSendToSession, `{"session_id":13,"message":" hello\nthere "}`, "sent to #25",
			"Message queued for session #25. No reply or completion signal comes back; do not poll.",
			hostCall{caller: 12, target: 13, message: " hello\nthere "},
		},
		{
			IDSpawnSession, `{"project":"api","worktree":"login","prompt":" solve login "}`, "spawned #26",
			"Started session #26 in api — login. No reply or completion signal comes back; do not poll.",
			hostCall{caller: 12, project: "api", worktree: "login", prompt: " solve login "},
		},
	} {
		t.Run(tc.id, func(t *testing.T) {
			host := &fakeHost{}
			result, err := findTool(t, host, tc.id).Execute(t.Context(), json.RawMessage(tc.args))
			require.NoError(t, err)
			assert.Equal(t, tc.title, result.Title)
			assert.Equal(t, tc.output, result.Output)
			assert.Equal(t, []hostCall{tc.call}, host.calls)
			host.err = errors.New("owner-specific refusal")
			_, err = findTool(t, host, tc.id).Execute(t.Context(), json.RawMessage(tc.args))
			require.ErrorIs(t, err, host.err)
			assert.EqualError(t, err, "owner-specific refusal")
		})
	}
}

func TestToolsDeclarations(t *testing.T) {
	tools := NewTools(nil, 12)
	require.Len(t, tools, 3)
	for i, tc := range []struct{ id, trail, prompt string }{
		{IDListSessions, "filter", "list your root sessions"},
		{IDSendToSession, "message", "write to a session"},
		{IDSpawnSession, "prompt", "start a session in a project"},
	} {
		candidate := tools[i]
		assert.Equal(t, tc.id, candidate.ID())
		assert.False(t, candidate.ParallelSafe())
		trail, ok := candidate.(tool.TrailDeclarer)
		require.True(t, ok)
		assert.Equal(t, tool.TrailArgument{Param: tc.trail}, trail.TrailArgument())
		prompt, ok := candidate.(tool.PromptDeclarer)
		require.True(t, ok)
		assert.Equal(
			t,
			tool.PromptDeclaration{Group: tool.PromptGroupSessions, Order: i + 1, Description: tc.prompt},
			prompt.PromptDeclaration(),
		)
		assert.NotEmpty(t, candidate.Description())
		var schema map[string]any
		require.NoError(t, json.Unmarshal(candidate.Parameters(), &schema))
		assert.Equal(t, "object", schema["type"])
	}
}

func TestRenderSessions(t *testing.T) {
	now := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{
			ID:       12,
			Name:     "coagent · fix flaky tests",
			State:    "idle",
			Activity: now.Add(-5*time.Minute - 59*time.Second),
			Self:     true,
		},
		{ID: 13, Name: "api", State: "running", Activity: now.Add(-90 * time.Minute)},
		{ID: 14, Name: "docs", State: "stopped", Activity: now.Add(-49 * time.Hour)},
		{ID: 15, Name: "future", State: "parked", Activity: now.Add(time.Minute)},
	}
	assert.Equal(t, "#12 coagent · fix flaky tests — idle, active 5m ago (this session)\n"+
		"#13 api — running, active 1h ago\n#14 docs — stopped, active 2d ago\n#15 future — parked, active 0m ago",
		renderSessions(entries, 4, "", now))
	assert.Equal(t, "No sessions.", renderSessions(nil, 0, "", now))
	assert.Equal(t, `No sessions match "unknown".`, renderSessions(nil, 0, "unknown", now))
	many := make([]Entry, 20)
	for i := range many {
		many[i] = entries[i%len(entries)]
	}
	listed := renderSessions(many, 21, "", now)
	assert.Equal(t, renderSessions(many, 20, "", now)+"\n… and 1 more; narrow with filter", listed)
	assert.Equal(t, 20, strings.Count(listed, "\n"))
}

func findTool(t *testing.T, host Host, id string) tool.Tool {
	t.Helper()
	for _, candidate := range NewTools(host, 12) {
		if candidate.ID() == id {
			return candidate
		}
	}
	t.Fatalf("tool %s is missing", id)
	return nil
}
