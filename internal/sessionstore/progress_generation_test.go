package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "hello")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "hello")
	require.NoError(t, err)

	// Assistant message output.
	_, commit, err := store.InsertAssistantMessageWithOutput(ctx, session.ID,
		&transcript.Message{Role: "assistant", Content: "working", ToolCalls: []byte(`[]`)},
		OutputMessageReplaceable, "working")
	require.NoError(t, err)
	require.NotZero(t, commit.OutputID)
	assert.InDelta(t, float64(1), ownerAttrs(t, db, commit.OutputID)["model_input_generation"].(float64), 0)

	// Direct output rides a tool result insertion.
	_, outputs, err := store.InsertToolResultWithDirectOutput(ctx, session.ID,
		&transcript.Message{Role: "tool", Content: "done", ToolCallID: "c1", ToolName: "bash"},
		[]string{"direct!"})
	require.NoError(t, err)
	require.Len(t, outputs, 1)
	assert.InDelta(t, float64(1), ownerAttrs(t, db, outputs[0].OutputID)["model_input_generation"].(float64), 0)

	// Assistant replaceable output via EnqueueOutput.
	draft := OutputDraft{
		SessionID: session.ID, Type: OutputMessageReplaceable, Content: "still working",
		SourceKey: "k1", Fingerprint: OutputFingerprint(OutputMessageReplaceable, "still working", session.ID, nil),
	}
	commit, err = store.EnqueueOutput(ctx, draft)
	require.NoError(t, err)
	assert.InDelta(t, float64(1), ownerAttrs(t, db, commit.OutputID)["model_input_generation"].(float64), 0)

	// Lifecycle outputs carry no generation.
	opened, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
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

	draft := OutputDraft{
		SessionID: session.ID, Type: OutputMessageReplaceable, Content: "card",
		SourceKey:   "progress:change:x:g0",
		Fingerprint: OutputFingerprint(OutputMessageReplaceable, "card", session.ID, nil),
	}
	commit, err := store.EnqueueOutput(ctx, draft)
	require.NoError(t, err)
	assert.InDelta(t, float64(0), ownerAttrs(t, db, commit.OutputID)["model_input_generation"].(float64), 0)

	// Advance the generation, then replay the same source key.
	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "next")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "next")
	require.NoError(t, err)

	replay, err := store.EnqueueOutput(ctx, draft)
	require.NoError(t, err)
	assert.Equal(t, commit.OutputID, replay.OutputID)
	assert.True(t, replay.Existing)
	assert.InDelta(t, float64(0), ownerAttrs(t, db, replay.OutputID)["model_input_generation"].(float64), 0)
}

func TestProducerCannotSetGenerationAttribute(t *testing.T) {
	draft := OutputDraft{
		SessionID: 1, Type: OutputMessagePersistent, Content: "x",
		Attributes: map[string]any{ModelInputGenerationAttribute: int64(7)},
	}
	err := validateOutputDraft(draft)
	require.ErrorContains(t, err, "model_input_generation")
}

func TestEnqueueProgressOutputSuperseded(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	draft := func() OutputDraft {
		return OutputDraft{
			SessionID: session.ID, Type: OutputMessageReplaceable, Content: "card",
			SourceKey:   "progress:change:m:g0",
			Fingerprint: OutputFingerprint(OutputMessageReplaceable, "card", session.ID, nil),
		}
	}

	// Generation supersession.
	commit, err := store.EnqueueProgressOutput(ctx, draft(), 0, SessionStatusActive)
	require.NoError(t, err)
	require.NotZero(t, commit.OutputID)

	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "next")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "next")
	require.NoError(t, err)

	stale := OutputDraft{
		SessionID: session.ID, Type: OutputMessageReplaceable, Content: "stale card",
		SourceKey:   "progress:change:m2:g0",
		Fingerprint: OutputFingerprint(OutputMessageReplaceable, "stale card", session.ID, nil),
	}
	_, err = store.EnqueueProgressOutput(ctx, stale, 0, SessionStatusActive)
	require.ErrorIs(t, err, ErrProgressSuperseded)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM session_outbox WHERE source_key = ?`,
		stale.SourceKey).Scan(&count))
	assert.Equal(t, 0, count, "superseded snapshot inserts nothing")

	// Current generation succeeds.
	fresh := stale
	fresh.SourceKey = "progress:change:m2:g1"
	fresh.Fingerprint = OutputFingerprint(OutputMessageReplaceable, "stale card", session.ID, nil)
	commit, err = store.EnqueueProgressOutput(ctx, fresh, 1, SessionStatusActive)
	require.NoError(t, err)
	require.NotZero(t, commit.OutputID)

	// Status supersession.
	_, err = db.Exec(`UPDATE sessions SET status = 'stopping' WHERE id = ?`, session.ID)
	require.NoError(t, err)

	_, err = store.EnqueueProgressOutput(ctx, OutputDraft{
		SessionID: session.ID, Type: OutputMessageReplaceable, Content: "late card",
		SourceKey:   "progress:change:m3:g1",
		Fingerprint: OutputFingerprint(OutputMessageReplaceable, "late card", session.ID, nil),
	}, 1, SessionStatusActive)
	require.ErrorIs(t, err, ErrProgressSuperseded)

	// Stopping status is never eligible even when expected verbatim.
	_, err = store.EnqueueProgressOutput(ctx, OutputDraft{
		SessionID: session.ID, Type: OutputMessageReplaceable, Content: "late card",
		SourceKey:   "progress:change:m4:g1",
		Fingerprint: OutputFingerprint(OutputMessageReplaceable, "late card", session.ID, nil),
	}, 1, SessionStatusStopping)
	require.ErrorIs(t, err, ErrProgressSuperseded)
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
	_, err = store.InsertMessage(ctx, session.ID, assistant("old narration", `[{"id":"1","name":"bash","input":{}}]`))
	require.NoError(t, err)

	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "go")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "go")
	require.NoError(t, err)

	// Tool-only row: no text.
	_, err = store.InsertMessage(ctx, session.ID, assistant("", `[{"id":"2","name":"bash","input":{}}]`))
	require.NoError(t, err)

	// Reasoning-only row: text empty, reasoning set.
	_, err = store.InsertMessage(ctx, session.ID, &transcript.Message{
		Role: "assistant", Content: "", ReasoningContent: "thinking",
		ToolCalls: jsonRaw(`[{"id":"3","name":"bash","input":{}}]`),
	})
	require.NoError(t, err)

	facts, err := store.CaptureProgress(ctx, session.ID)
	require.NoError(t, err)
	assert.Empty(t, facts.LatestModelProgress, "tool-only and reasoning-only rows are not notes")

	// Narrated tool row becomes the note.
	_, err = store.InsertMessage(
		ctx,
		session.ID,
		assistant("current narration", `[{"id":"4","name":"bash","input":{}}]`),
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
	_, err = store.ReplaceCompactedMessages(ctx, session.ID, []int64{noteID}, []CompactionEntry{})
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
	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "stop mutations")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, input.RawContent)
	require.NoError(t, err)

	_, output, err := store.InsertAssistantMessageWithOutput(ctx, session.ID, &transcript.Message{
		Role: "assistant", Content: "Stopping the mutation run",
		ToolCalls: jsonRaw(`[{"id":"stop","name":"bash","input":{}}]`),
	}, OutputMessagePersistent, "Stopping the mutation run")
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

	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "go")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "go")
	require.NoError(t, err)

	_, err = store.InsertMessage(ctx, session.ID, &transcript.Message{
		Role: "assistant", Content: "old narration",
		ToolCalls: jsonRaw(`[{"id":"1","name":"bash","input":{}}]`),
	})
	require.NoError(t, err)

	// A scheduled injection advances the boundary; its turn has no narration.
	_, inserted, err := store.ResetSessionContextOnce(ctx, session.ID, "sched-1", "fp-s1",
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
	foreground, err := store.CreateSubagentSession(ctx, projectID, root.ID, root.ID, "general", "m", "")
	require.NoError(t, err)
	nestedForeground, err := store.CreateSubagentSession(
		ctx, projectID, foreground, root.ID, "general", "m", "",
	)
	require.NoError(t, err)
	background, err := store.CreateSubagentSession(ctx, projectID, root.ID, root.ID, "general", "m", "")
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
