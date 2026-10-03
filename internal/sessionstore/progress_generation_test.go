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

func TestMessageOutputsStampGenerationLifecycleOutputsDoNot(t *testing.T) {
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
		sessionstore.OutputMessageReplaceable, "working", false)
	require.NoError(t, err)
	require.NotZero(t, commit.OutputID)
	assert.InDelta(t, float64(1), ownerAttrs(t, db, commit.OutputID)["model_input_generation"].(float64), 0)

	// Direct output rides a tool result insertion.
	_, outputs, err := answerTool(ctx, store, session.ID,
		&transcript.Message{Role: "tool", Content: "done", ToolCallID: "c1", ToolName: "bash"},
		[]string{"direct!"})
	require.NoError(t, err)
	require.Len(t, outputs, 1)
	assert.InDelta(t, float64(1), ownerAttrs(t, db, outputs[0].OutputID)["model_input_generation"].(float64), 0)

	// Assistant replaceable output via EnqueueOutput.
	draft := sessionstore.OutputDraft{
		SessionID: session.ID,
		Type:      sessionstore.OutputMessageReplaceable,
		Content:   "still working",
		SourceKey: "k1",
		Fingerprint: sessionstore.OutputFingerprint(
			sessionstore.OutputMessageReplaceable,
			"still working",
			session.ID,
			nil,
		),
	}
	commit, err = store.EnqueueOutput(ctx, draft)
	require.NoError(t, err)
	assert.InDelta(t, float64(1), ownerAttrs(t, db, commit.OutputID)["model_input_generation"].(float64), 0)

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

func TestSourceKeyReplayReturnsOriginalGeneration(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	draft := sessionstore.OutputDraft{
		SessionID: session.ID, Type: sessionstore.OutputMessageReplaceable, Content: "card",
		SourceKey:   "progress:change:x:g0",
		Fingerprint: sessionstore.OutputFingerprint(sessionstore.OutputMessageReplaceable, "card", session.ID, nil),
	}
	commit, err := store.EnqueueOutput(ctx, draft)
	require.NoError(t, err)
	assert.InDelta(t, float64(0), ownerAttrs(t, db, commit.OutputID)["model_input_generation"].(float64), 0)

	// Advance the generation, then replay the same source key.
	input, err := enqueueInput(ctx, store, session.ID, sessionstore.InputSourceUser, "next")
	require.NoError(t, err)
	_, err = acceptInput(ctx, store, input.ID, "next")
	require.NoError(t, err)

	replay, err := store.EnqueueOutput(ctx, draft)
	require.NoError(t, err)
	assert.Equal(t, commit.OutputID, replay.OutputID)
	assert.True(t, replay.Existing)
	assert.InDelta(t, float64(0), ownerAttrs(t, db, replay.OutputID)["model_input_generation"].(float64), 0)
}

func TestProducerCannotSetGenerationAttribute(t *testing.T) {
	draft := sessionstore.OutputDraft{
		SessionID: 1, Type: sessionstore.OutputMessagePersistent, Content: "x",
		Attributes: map[string]any{sessionstore.ModelInputGenerationAttribute: int64(7)},
	}
	store, _, _ := newTestStore(t)
	_, err := store.EnqueueOutput(t.Context(), draft)
	require.ErrorContains(t, err, "model_input_generation")
}

func TestEnqueueProgressOutputSuperseded(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	draft := func() sessionstore.OutputDraft {
		return sessionstore.OutputDraft{
			SessionID: session.ID, Type: sessionstore.OutputMessageReplaceable, Content: "card",
			SourceKey:   "progress:change:m:g0",
			Fingerprint: sessionstore.OutputFingerprint(sessionstore.OutputMessageReplaceable, "card", session.ID, nil),
		}
	}

	// Generation supersession.
	commit, err := store.EnqueueProgressOutput(ctx, draft(), 0, sessionstore.SessionStatusActive)
	require.NoError(t, err)
	require.NotZero(t, commit.OutputID)

	input, err := enqueueInput(ctx, store, session.ID, sessionstore.InputSourceUser, "next")
	require.NoError(t, err)
	_, err = acceptInput(ctx, store, input.ID, "next")
	require.NoError(t, err)

	stale := sessionstore.OutputDraft{
		SessionID: session.ID,
		Type:      sessionstore.OutputMessageReplaceable,
		Content:   "stale card",
		SourceKey: "progress:change:m2:g0",
		Fingerprint: sessionstore.OutputFingerprint(
			sessionstore.OutputMessageReplaceable,
			"stale card",
			session.ID,
			nil,
		),
	}
	_, err = store.EnqueueProgressOutput(ctx, stale, 0, sessionstore.SessionStatusActive)
	require.ErrorIs(t, err, sessionstore.ErrProgressSuperseded)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM session_outbox WHERE source_key = ?`,
		stale.SourceKey).Scan(&count))
	assert.Equal(t, 0, count, "superseded snapshot inserts nothing")

	// Current generation succeeds.
	fresh := stale
	fresh.SourceKey = "progress:change:m2:g1"
	fresh.Fingerprint = sessionstore.OutputFingerprint(
		sessionstore.OutputMessageReplaceable,
		"stale card",
		session.ID,
		nil,
	)
	commit, err = store.EnqueueProgressOutput(ctx, fresh, 1, sessionstore.SessionStatusActive)
	require.NoError(t, err)
	require.NotZero(t, commit.OutputID)

	// Status supersession.
	_, err = db.Exec(`UPDATE sessions SET status = 'stopping' WHERE id = ?`, session.ID)
	require.NoError(t, err)

	_, err = store.EnqueueProgressOutput(ctx, sessionstore.OutputDraft{
		SessionID: session.ID,
		Type:      sessionstore.OutputMessageReplaceable,
		Content:   "late card",
		SourceKey: "progress:change:m3:g1",
		Fingerprint: sessionstore.OutputFingerprint(
			sessionstore.OutputMessageReplaceable,
			"late card",
			session.ID,
			nil,
		),
	}, 1, sessionstore.SessionStatusActive)
	require.ErrorIs(t, err, sessionstore.ErrProgressSuperseded)

	// Stopping status is never eligible even when expected verbatim.
	_, err = store.EnqueueProgressOutput(ctx, sessionstore.OutputDraft{
		SessionID: session.ID,
		Type:      sessionstore.OutputMessageReplaceable,
		Content:   "late card",
		SourceKey: "progress:change:m4:g1",
		Fingerprint: sessionstore.OutputFingerprint(
			sessionstore.OutputMessageReplaceable,
			"late card",
			session.ID,
			nil,
		),
	}, 1, sessionstore.SessionStatusStopping)
	require.ErrorIs(t, err, sessionstore.ErrProgressSuperseded)
}

func TestCaptureProgressNoteScoping(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	assistant := func(content, toolCalls string) *transcript.Message {
		return &transcript.Message{Role: "assistant", Content: content, ToolCalls: jsonRaw(toolCalls)}
	}

	// Pre-boundary narration from an old turn.
	_, err = appendMessage(ctx, store, session.ID, assistant("old narration", `[{"ID":"1","Name":"bash","input":{}}]`))
	require.NoError(t, err)

	input, err := enqueueInput(ctx, store, session.ID, sessionstore.InputSourceUser, "go")
	require.NoError(t, err)
	_, err = acceptInput(ctx, store, input.ID, "go")
	require.NoError(t, err)

	// Tool-only row: no text.
	_, err = appendMessage(ctx, store, session.ID, assistant("", `[{"ID":"2","Name":"bash","input":{}}]`))
	require.NoError(t, err)

	// Reasoning-only row: text empty, reasoning set.
	_, err = appendMessage(ctx, store, session.ID, &transcript.Message{
		Role: "assistant", Content: "", ReasoningContent: "thinking",
		ToolCalls: jsonRaw(`[{"ID":"3","Name":"bash","input":{}}]`),
	})
	require.NoError(t, err)

	facts, err := store.CaptureProgress(ctx, session.ID)
	require.NoError(t, err)
	assert.Empty(t, facts.LatestModelProgress, "tool-only and reasoning-only rows are not notes")

	// Narrated tool row becomes the note.
	_, err = appendMessage(ctx, store,
		session.ID,
		assistant("current narration", `[{"ID":"4","Name":"bash","input":{}}]`),
	)
	require.NoError(t, err)
	facts, err = store.CaptureProgress(ctx, session.ID)
	require.NoError(t, err)
	assert.Equal(t, "current narration", facts.LatestModelProgress)
	assert.Equal(t, int64(1), facts.ModelInputGeneration)
	assert.NotZero(t, facts.ModelInputBoundary)

	// Compacted rows never supply the note.
	var noteID int64
	require.NoError(t, db.QueryRow(`SELECT id FROM messages WHERE content = 'current narration'`).Scan(&noteID))
	_, err = replaceTranscript(ctx, store, session.ID, []int64{noteID}, []sessionstore.CompactionEntry{})
	require.NoError(t, err)
	facts, err = store.CaptureProgress(ctx, session.ID)
	require.NoError(t, err)
	assert.Empty(t, facts.LatestModelProgress, "compacted narration is dropped")
}

func TestCaptureProgressExcludesPublishedDirectReply(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)
	input, err := enqueueInput(ctx, store, session.ID, sessionstore.InputSourceUser, "stop mutations")
	require.NoError(t, err)
	_, err = acceptInput(ctx, store, input.ID, input.RawContent)
	require.NoError(t, err)

	_, output, err := appendPublished(ctx, store, session.ID, &transcript.Message{
		Role: "assistant", Content: "Stopping the mutation run",
		ToolCalls: jsonRaw(`[{"ID":"stop","Name":"bash","input":{}}]`),
	}, sessionstore.OutputMessagePersistent, "Stopping the mutation run", false)
	require.NoError(t, err)
	require.NotNil(t, output)

	var sourceKey string
	var releasesInput bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT source_key, releases_input
		FROM session_outbox WHERE id = ?`, output.OutputID).Scan(&sourceKey, &releasesInput))
	assert.Contains(t, sourceKey, ":reply")
	assert.False(t, releasesInput)

	facts, err := store.CaptureProgress(ctx, session.ID)
	require.NoError(t, err)
	assert.Empty(t, facts.LatestModelProgress)
}

func TestScheduledTurnWithoutNarrationDoesNotReuseNote(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	input, err := enqueueInput(ctx, store, session.ID, sessionstore.InputSourceUser, "go")
	require.NoError(t, err)
	_, err = acceptInput(ctx, store, input.ID, "go")
	require.NoError(t, err)

	_, err = appendMessage(ctx, store, session.ID, &transcript.Message{
		Role: "assistant", Content: "old narration",
		ToolCalls: jsonRaw(`[{"ID":"1","Name":"bash","input":{}}]`),
	})
	require.NoError(t, err)

	// A scheduled injection advances the boundary; its turn has no narration.
	_, inserted, err := freshTurn(ctx, store, session.ID, "sched-1", "fp-s1",
		[]*transcript.Message{{Role: "user", Content: "scheduled turn"}})
	require.NoError(t, err)
	require.True(t, inserted)

	facts, err := store.CaptureProgress(ctx, session.ID)
	require.NoError(t, err)
	assert.Empty(t, facts.LatestModelProgress, "the prior turn's note must not carry forward")
}

func TestCaptureProgressCountsActiveSubagentsAcrossRootTree(t *testing.T) {
	t.Parallel()

	store, db, projectID := newTestStore(t)
	ctx := t.Context()

	root, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	foreground, err := createChild(ctx, store, projectID, root.ID, root.ID, "general", "m", "")
	require.NoError(t, err)
	nestedForeground, err := createChild(ctx, store, projectID, foreground, root.ID, "general", "m", "")
	require.NoError(t, err)
	background, err := createChild(ctx, store, projectID, root.ID, root.ID, "general", "m", "")
	require.NoError(t, err)

	links := subagent.NewStore(db)
	require.NoError(t, links.InsertSubagentLink(ctx, subagent.Link{
		ParentID: root.ID, ChildID: foreground, TaskCallID: "fg", Blocking: true,
	}))
	require.NoError(t, links.InsertSubagentLink(ctx, subagent.Link{
		ParentID: foreground, ChildID: nestedForeground, TaskCallID: "nested-fg", Blocking: true,
	}))
	require.NoError(t, links.InsertSubagentLink(ctx, subagent.Link{
		ParentID: root.ID, ChildID: background, TaskCallID: "bg", Blocking: false,
	}))

	facts, err := store.CaptureProgress(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, facts.ActiveSubagents)
	assert.Equal(t, 1, facts.BackgroundSubagents)
	assert.Len(t, facts.Waiting, 1, "only a foreground child directly blocking the root is a root wait")
}

// A pending completion-check candidate is the model's newest statement: while
// it waits for the confirm turn, its text is the honest Working-card note.
func TestCaptureProgressNotePrefersPendingCandidate(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	input, err := enqueueInput(ctx, store, session.ID, sessionstore.InputSourceUser, "go")
	require.NoError(t, err)
	_, err = acceptInput(ctx, store, input.ID, "go")
	require.NoError(t, err)

	// The current turn's narration (tool-bearing) and then the candidate stop.
	_, err = appendMessage(ctx, store, session.ID, &transcript.Message{
		Role: "assistant", Content: "current narration",
		ToolCalls: jsonRaw(`[{"ID":"1","Name":"bash","input":{}}]`),
	})
	require.NoError(t, err)
	candidate, err := appendMessage(ctx, store, session.ID, &transcript.Message{
		Role: "assistant", Content: "full candidate answer", ToolCalls: jsonRaw(`[]`),
	})
	require.NoError(t, err)

	setCandidate := func(id int64) {
		// The column is FK-bound to messages; "cleared" is NULL, never 0.
		var value any
		if id != 0 {
			value = id
		}

		_, execErr := db.ExecContext(ctx,
			`UPDATE sessions SET completion_check_candidate_id = ? WHERE id = ?`, value, session.ID)
		require.NoError(t, execErr)
	}

	setCandidate(candidate)
	facts, err := store.CaptureProgress(ctx, session.ID)
	require.NoError(t, err)
	assert.Equal(t, "full candidate answer", facts.LatestModelProgress,
		"a pending candidate outranks the turn's tool narration")

	// Cleared candidate: note returns to the tool-bearing narration.
	setCandidate(0)
	facts, err = store.CaptureProgress(ctx, session.ID)
	require.NoError(t, err)
	assert.Equal(t, "current narration", facts.LatestModelProgress, "no stale candidate note")

	// A candidate behind the generation boundary is never the note. Moving the
	// boundary to the candidate id (as a promoted input would) puts the whole
	// prior turn behind it: neither the old candidate nor the old narration may
	// serve the new turn.
	_, err = db.ExecContext(ctx, `UPDATE sessions SET model_input_boundary = ? WHERE id = ?`,
		candidate, session.ID)
	require.NoError(t, err)
	facts, err = store.CaptureProgress(ctx, session.ID)
	require.NoError(t, err)
	assert.Empty(t, facts.LatestModelProgress,
		"a candidate behind the generation boundary is not the note")
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
