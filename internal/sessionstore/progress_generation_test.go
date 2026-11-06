package sessionstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

func ownerAttrs(t *testing.T, db *sql.DB, outputID int64) map[string]any {
	t.Helper()

	var encoded string
	require.NoError(t, db.QueryRow(`SELECT attributes FROM session_outbox WHERE id = ?`,
		outputID).Scan(&encoded))

	var attrs map[string]any
	require.NoError(t, json.Unmarshal([]byte(encoded), &attrs))

	return attrs
}

func jsonRaw(value string) json.RawMessage {
	return json.RawMessage(value)
}

func TestMessageAndLifecycleOutputsDoNotStampGeneration(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	input, err := enqueueInput(ctx, store, session.ID, sessionstore.InputSourceUser, "hello")
	require.NoError(t, err)
	_, err = acceptInput(ctx, store, input.ID, "hello")
	require.NoError(t, err)

	// Assistant message output.
	_, commit, err := appendPublished(ctx, store, session.ID,
		&transcript.Message{Role: "assistant", Content: "working", ToolCalls: []byte(`[]`)},
		sessionstore.OutputMessage, "working", false)
	require.NoError(t, err)
	require.NotZero(t, commit.OutputID)
	assert.NotContains(t, ownerAttrs(t, db, commit.OutputID), "model_input_generation")

	// Direct output rides a tool result insertion.
	_, outputs, err := answerTool(ctx, store, session.ID,
		&transcript.Message{Role: "tool", Content: "done", ToolCallID: "c1", ToolName: "bash"},
		[]string{"direct!"})
	require.NoError(t, err)
	require.Len(t, outputs, 1)
	assert.NotContains(t, ownerAttrs(t, db, outputs[0].OutputID), "model_input_generation")

	// Assistant replaceable output via EnqueueOutput.
	draft := sessionstore.OutputDraft{
		SessionID: session.ID,
		Type:      sessionstore.OutputMessage,
		Content:   "still working",
		SourceKey: "k1",
	}
	commit, err = store.EnqueueOutput(ctx, draft)
	require.NoError(t, err)
	assert.NotContains(t, ownerAttrs(t, db, commit.OutputID), "model_input_generation")

	// Lifecycle outputs carry no generation.
	opened, _, err := store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Attributes: map[string]any{"manager_id": "mgr"},
		Name: "n", WorkDir: "/tmp/wd", Prompt: "go",
	})
	require.NoError(t, err)
	var openedAttrs string
	require.NoError(t, db.QueryRow(`SELECT attributes FROM session_outbox
		WHERE session_id = ? AND type = 'session_opened'`, opened.ID).Scan(&openedAttrs))
	assert.NotContains(t, openedAttrs, "model_input_generation")
}

func TestSourceKeyReplaySurvivesGenerationChange(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	draft := sessionstore.OutputDraft{
		SessionID: session.ID, Type: sessionstore.OutputMessage, Content: "card",
		SourceKey: "progress:change:x:g0",
	}
	commit, err := store.EnqueueOutput(ctx, draft)
	require.NoError(t, err)
	assert.NotContains(t, ownerAttrs(t, db, commit.OutputID), "model_input_generation")

	// Advance the generation, then replay the same source key.
	input, err := enqueueInput(ctx, store, session.ID, sessionstore.InputSourceUser, "next")
	require.NoError(t, err)
	_, err = acceptInput(ctx, store, input.ID, "next")
	require.NoError(t, err)
	state, err := testStore(db).LoadCompletionCheckState(ctx, session.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), state.ModelInputGeneration)

	replay, err := store.EnqueueOutput(ctx, draft)
	require.NoError(t, err)
	assert.Equal(t, commit.OutputID, replay.OutputID)
	assert.True(t, replay.Existing)
	assert.NotContains(t, ownerAttrs(t, db, replay.OutputID), "model_input_generation")
}

func TestProducerCannotSetGenerationAttribute(t *testing.T) {
	draft := sessionstore.OutputDraft{
		SessionID: 1, Type: sessionstore.OutputMessage, Content: "x",
		Attributes: map[string]any{sessionstore.ModelInputGenerationAttribute: int64(7)},
	}
	store, _, _ := newTestStore(t)
	_, err := store.EnqueueOutput(t.Context(), draft)
	require.ErrorContains(t, err, "model_input_generation")
}

func TestCaptureProgressCountsActiveSubagentsAcrossRootTree(t *testing.T) {
	t.Parallel()

	store, db, projectID := newTestStore(t)
	ctx := t.Context()

	root, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	links := subagent.NewStore(db, store)
	foreground, err := links.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: root.ID, RootID: root.ID, AgentType: "general", Model: "m",
		TaskCallID: "fg", Blocking: true, State: subagent.StateSpawned,
	})
	require.NoError(t, err)
	_, err = links.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: foreground, RootID: root.ID, AgentType: "general", Model: "m",
		TaskCallID: "nested-fg", Blocking: true, State: subagent.StateSpawned,
	})
	require.NoError(t, err)
	_, err = links.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: root.ID, RootID: root.ID, AgentType: "general", Model: "m",
		TaskCallID: "bg", State: subagent.StateSpawned,
	})
	require.NoError(t, err)

	facts, err := store.CaptureProgress(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, facts.ActiveSubagents)
	assert.Equal(t, 1, facts.BackgroundSubagents)
	assert.Len(t, facts.Waiting, 1, "only a foreground child directly blocking the root is a root wait")
}

// LoadCompletionCheckState resolves the pending candidate's text alongside its
// id, so the confirming disposition can publish the candidate instead of the
// nudge ack without a second query.
func TestLoadCompletionCheckStateCarriesCandidateText(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	candidateID, err := appendMessage(ctx, store, session.ID, &transcript.Message{
		Role: "assistant", Content: "full candidate answer", ToolCalls: jsonRaw(`[]`),
	})
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`UPDATE sessions SET completion_check_candidate_id = ? WHERE id = ?`, candidateID, session.ID)
	require.NoError(t, err)

	state, err := store.LoadCompletionCheckState(ctx, session.ID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	assert.Equal(t, candidateID, *state.CandidateID)
	assert.Equal(t, "full candidate answer", state.CandidateText)

	// No pending candidate: text is empty.
	_, err = db.ExecContext(ctx,
		`UPDATE sessions SET completion_check_candidate_id = NULL WHERE id = ?`, session.ID)
	require.NoError(t, err)

	state, err = store.LoadCompletionCheckState(ctx, session.ID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID)
	assert.Empty(t, state.CandidateText)
}
