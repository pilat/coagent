package sessionstore

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHumanInputObservationTargets(t *testing.T) {
	for _, kind := range []string{
		"text", "/budget 5$", "/skill review", "/status", "/help", "/schedules", "/compact args",
		"/stop", "/clear", "/kill", "assistant input", "assistant root", "subagent", "other owner", "no assistant", "killed assistant",
	} {
		t.Run(kind, func(t *testing.T) {
			f := newHumanObserverFixture(t)
			input := Input{SessionID: f.work, Source: InputSourceUser, Content: kind}
			want := kind == "text" || kind == "/budget 5$" || kind == "/skill review"
			switch kind {
			case "assistant input":
				input.Attributes = map[string]any{"from": "assistant", ObserverSessionIDAttribute: f.observer}
				want = true
			case "assistant root":
				input.SessionID = f.observer
			case "subagent":
				child, err := f.store.CreateSubagentSession(
					t.Context(),
					f.project,
					f.work,
					f.work,
					"general",
					"model",
					"",
				)
				require.NoError(t, err)
				input.SessionID = child
			case "other owner", "no assistant":
				require.NoError(t, f.store.SetAttributes(t.Context(), f.work, map[string]any{"manager_id": "other"}))
			case "killed assistant":
				_, err := f.db.ExecContext(
					t.Context(),
					`UPDATE sessions SET killed_at = CURRENT_TIMESTAMP WHERE id = ?`,
					f.observer,
				)
				require.NoError(t, err)
			}
			in, err := f.store.Enqueue(t.Context(), input)
			require.NoError(t, err)
			if want {
				assert.EqualValues(t, f.observer, in.Input.Attributes[ObserverSessionIDAttribute])
			} else {
				assert.NotContains(t, in.Input.Attributes, ObserverSessionIDAttribute)
			}
			if kind != "assistant input" {
				assert.True(t, IsHumanInput(in.Input.Source, in.Input.Attributes))
			}
		})
	}
}

func TestObserverHumanExchange(t *testing.T) {
	f := newHumanObserverFixture(t)
	f.human(t, "first question", true)
	f.ref(t, true)
	f.final(t, "intermediate", true)
	f.human(t, "direct follow-up", true)
	f.store = NewStore(f.db)
	f.final(t, "final answer", false)
	events := f.events(t)
	require.Len(t, events, 1)
	assert.Equal(t, fmt.Sprintf(
		"→ test (%d) user wrote: %q\n→ test (%d) user wrote: %q\n← test (%d) answered: %q",
		f.work,
		"first question",
		f.work,
		"direct follow-up",
		f.work,
		"intermediate\n\n---\n\nfinal answer",
	), events[0].RawContent)
	f.final(t, "unrelated final", false)
	assert.Len(t, f.events(t), 1)
	f.human(t, "new exchange", true)
	f.final(t, "new answer", false)
	events = f.events(t)
	require.Len(t, events, 2)
	assert.NotContains(t, events[1].RawContent, "first question")
	assert.Contains(t, events[1].RawContent, `user wrote: "new exchange"`)
}

func TestObserverHumanFailureAndClipping(t *testing.T) {
	f := newHumanObserverFixture(t)
	question := "HEAD" + strings.Repeat("я", 2500) + "TAIL"
	f.human(t, question, true)
	f.human(t, "pending question", false)
	_, err := f.store.RecordSessionStartFailure(t.Context(), f.work, "cannot start")
	require.NoError(t, err)
	events := f.events(t)
	require.Len(t, events, 1)
	assert.Equal(
		t,
		fmt.Sprintf("→ test (%d) user wrote: %q\n→ test (%d) user wrote: %q\n← test (%d) failed: cannot start",
			f.work, ClipMiddle(question, 2000), f.work, "pending question", f.work),
		events[0].RawContent,
	)
	assert.Contains(t, events[0].RawContent, "HEAD")
	assert.Contains(t, events[0].RawContent, "TAIL")
	assert.NotContains(t, events[0].RawContent, strings.Repeat("я", 2000))
}

func TestObservedHumanRetainsGrantAndBudgetLift(t *testing.T) {
	f := newHumanObserverFixture(t)
	mutation := targetedBudgetMutation(t, f.store, f.work, f.work, "arm")
	armed, err := f.store.Arm(t.Context(), mutation)
	require.NoError(t, err)
	fired, _, err := f.store.FireBudget(t.Context(), f.work, armed.Generation, "duration", 0, "checkpoint")
	require.NoError(t, err)
	_, err = f.store.BeginBudgetDrain(t.Context(), f.work, fired.Generation, fired.ParkOwner)
	require.NoError(t, err)
	_, err = f.store.MarkBudgetParked(t.Context(), f.work, fired.Generation, fired.ParkOwner)
	require.NoError(t, err)
	in := f.human(t, "/budget 5$", false)
	_, _, err = acceptActivation(
		t.Context(),
		f.store,
		in,
		"/budget 5$",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"},
	)
	require.NoError(t, err)
	grant, err := f.store.PendingActivation(t.Context(), f.work)
	require.NoError(t, err)
	assert.Equal(t, in, grant.InputID)
	record, err := f.store.Get(t.Context(), f.work)
	require.NoError(t, err)
	assert.Equal(t, "released", string(record.State))
}

func newHumanObserverFixture(t *testing.T) observerFixture {
	t.Helper()
	f := newObserverFixture(t)
	root, err := f.store.EnsureAssistantRoot(t.Context(), f.project, "manager", "/assistant", "model", "")
	require.NoError(t, err)
	f.observer = root.ID
	return f
}

func TestObserverReportedSchedule(t *testing.T) {
	f := newHumanObserverFixture(t)
	f.human(t, "before schedule", true)
	in, err := f.store.Enqueue(t.Context(), Input{
		SessionID: f.work, Source: InputSourceSchedule, Content: "scheduled work",
		Attributes: map[string]any{ObserverSessionIDAttribute: f.observer, "schedule_id": int64(123)},
	})
	require.NoError(t, err)
	_, err = acceptInput(t.Context(), f.store, in.Input.ID, "scheduled work")
	require.NoError(t, err)
	f.human(t, "after schedule", true)
	f.final(t, "scheduled answer", false)
	events := f.events(t)
	require.Len(t, events, 1)
	assert.Equal(
		t,
		fmt.Sprintf(
			"→ test (%d) user wrote: %q\n⏰ test (%d) schedule #123 fired\n→ test (%d) user wrote: %q\n← test (%d) answered: %q",
			f.work,
			"before schedule",
			f.work,
			f.work,
			"after schedule",
			f.work,
			"scheduled answer",
		),
		events[0].RawContent,
	)
	assert.NotContains(t, events[0].RawContent, `user wrote: "scheduled work"`)
}

func (f observerFixture) human(t *testing.T, text string, accepted bool) int64 {
	t.Helper()
	in, err := f.store.Enqueue(t.Context(), Input{SessionID: f.work, Source: InputSourceUser, Content: text})
	require.NoError(t, err)
	require.EqualValues(t, f.observer, in.Input.Attributes[ObserverSessionIDAttribute])
	if accepted {
		_, err = acceptInput(t.Context(), f.store, in.Input.ID, text)
		require.NoError(t, err)
	}
	return in.Input.ID
}
