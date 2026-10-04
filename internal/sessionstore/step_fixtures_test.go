package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/transcript"
)

func enqueueInput(ctx context.Context, s *Store, id int64, source InputSource, content string) (*InboxInput, error) {
	result, err := s.Enqueue(ctx, Input{SessionID: id, Source: source, Content: content})
	if err != nil || result == nil {
		return nil, err
	}
	return result.Input, nil
}

func enqueueFact(ctx context.Context, s *Store,
	id int64,
	source InputSource,
	content string,
	attrs map[string]any,
) (*InboxInput, error) {
	result, err := s.Enqueue(ctx, Input{SessionID: id, Source: source, Content: content, Attributes: attrs})
	if err != nil || result == nil {
		return nil, err
	}
	return result.Input, nil
}

func enqueueUser(ctx context.Context, s *Store, id int64, content string) (*InboxInput, error) {
	return enqueueInput(ctx, s, id, InputSourceUser, content)
}

func appendMessages(ctx context.Context, s *Store, id int64, messages []*transcript.Message) ([]int64, error) {
	result, err := s.Commit(ctx, Commit{SessionID: id, Messages: messages})
	if err != nil {
		return nil, err
	}
	return result.MessageIDs, nil
}

func appendMessage(ctx context.Context, s *Store, id int64, message *transcript.Message) (int64, error) {
	ids, err := appendMessages(ctx, s, id, []*transcript.Message{message})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

func acceptInput(ctx context.Context, s *Store, inputID int64, content string) (*transcript.Message, error) {
	in, err := inputFixture(ctx, s, inputID)
	if err != nil {
		return nil, err
	}
	result, err := s.Commit(
		ctx,
		Commit{
			SessionID: in.SessionID,
			Accept: []Accept{
				{InputID: inputID, State: InputStateAccepted, Content: content, LinkRef: -1, ModelBound: true},
			},
		},
	)
	if err != nil {
		return nil, err
	}
	messages, err := s.LoadActiveMessages(ctx, in.SessionID)
	if err != nil {
		return nil, err
	}
	for _, msg := range messages {
		if msg.ID == result.MessageIDs[0] {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("accepted message missing")
}

func acceptActivation(ctx context.Context, s *Store,
	inputID int64,
	content string,
	draft ActivationDraft,
) (*transcript.Message, *ToolActivation, error) {
	in, err := inputFixture(ctx, s, inputID)
	if err != nil {
		return nil, nil, err
	}
	result, err := s.Commit(
		ctx,
		Commit{
			SessionID: in.SessionID,
			Accept: []Accept{
				{InputID: inputID, State: InputStateAccepted, Content: content, LinkRef: -1, ModelBound: true},
			},
			Activation: &ActivationChange{InputID: inputID, ToolID: draft.ToolID, Command: draft.Command},
		},
	)
	if err != nil {
		return nil, nil, err
	}
	return &transcript.Message{ID: result.MessageIDs[0]}, result.Activation, nil
}

func resolveHandled(ctx context.Context, s *Store, inputID int64, reason string) error {
	in, err := inputFixture(ctx, s, inputID)
	if err != nil {
		return err
	}
	_, err = s.Commit(
		ctx,
		Commit{
			SessionID: in.SessionID,
			Accept:    []Accept{{InputID: inputID, State: InputStateHandled, Reason: reason, LinkRef: -1}},
		},
	)
	return err
}

func stepState(ctx context.Context, s *Store, id int64, iteration int, status SessionStatus) error {
	_, err := s.Commit(ctx, Commit{SessionID: id, State: StatePatch{Iteration: &iteration, Status: &status}})
	return err
}

func createChild(ctx context.Context, s *Store,
	project, parent, root int64,
	agent, model, reasoning string,
) (int64, error) {
	var id int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = CreateSubagentSessionTx(
			ctx,
			tx,
			CreateSubagentSession{
				ProjectID:      project,
				ParentID:       parent,
				RootID:         root,
				AgentType:      agent,
				Model:          model,
				ReasoningLevel: reasoning,
			},
		)
		return err
	})
	return id, err
}

func replaceTranscript(ctx context.Context, s *Store,
	id int64,
	head []int64,
	entries []CompactionEntry,
) ([]int64, error) {
	_, err := s.Commit(ctx, Commit{SessionID: id, Replace: &Replace{HeadIDs: head, Entries: entries}})
	if err != nil {
		return nil, err
	}
	messages, err := s.LoadActiveMessages(ctx, id)
	var ids []int64
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	return ids, err
}

func compactHead(ctx context.Context, s *Store, head []int64) error {
	if len(head) == 0 {
		return nil
	}
	var id int64
	err := sDB(s).QueryRowContext(ctx, "SELECT session_id FROM messages WHERE id = ?", head[0]).Scan(&id)
	if err != nil {
		return err
	}
	_, err = s.Commit(ctx, Commit{SessionID: id, Replace: &Replace{HeadIDs: head}})
	return err
}

func appendPublished(ctx context.Context, s *Store,
	id int64,
	message *transcript.Message,
	kind OutputType,
	content string,
	releases bool,
) (int64, *OutputCommit, error) {
	result, err := s.Commit(
		ctx,
		Commit{
			SessionID: id,
			Messages:  []*transcript.Message{message},
			Outputs:   []Output{{Type: kind, Content: content, ReleasesInput: releases, MessageRef: 0, Phase: "reply"}},
		},
	)
	if err != nil {
		return 0, nil, err
	}
	var output *OutputCommit
	if len(result.Outputs) > 0 {
		output = result.Outputs[0]
	}
	return result.MessageIDs[0], output, nil
}

func answerTool(ctx context.Context, s *Store,
	id int64,
	message *transcript.Message,
	outputs []string,
) (int64, []*OutputCommit, error) {
	var parts []Output
	for i, draft := range outputs {
		parts = append(
			parts,
			Output{
				Type:          OutputMessagePersistent,
				Content:       draft,
				Key:           fmt.Sprintf("tool:%s:direct:%d", message.ToolCallID, i),
				ReleasesInput: true,
			},
		)
	}
	result, err := s.Commit(ctx, Commit{SessionID: id, ToolResults: []*transcript.Message{message}, Outputs: parts})
	if err != nil {
		return 0, nil, err
	}
	return result.MessageIDs[0], result.Outputs, nil
}

func scheduledTurn(ctx context.Context, s *Store,
	id int64,
	key, fingerprint string,
	assistant, result *transcript.Message,
) (int64, int64, bool, error) {
	enqueued, err := s.Enqueue(
		ctx,
		Input{SessionID: id, Source: InputSourceSchedule, Content: result.Content, DeliveryKey: key},
	)
	if err != nil {
		return 0, 0, false, err
	}
	if !enqueued.Applied {
		return 0, 0, false, nil
	}
	committed, err := s.Commit(
		ctx,
		Commit{
			SessionID: id,
			Messages:  []*transcript.Message{assistant, result},
			Outputs: []Output{
				{
					Type:       OutputMessagePersistent,
					Content:    result.Content,
					Key:        "schedule:" + key + ":announcement",
					Attributes: map[string]any{"source": "scheduler"},
				},
			},
			Accept: []Accept{{InputID: enqueued.Input.ID, State: InputStateAccepted, LinkRef: 1, ModelBound: true}},
		},
	)
	if err != nil {
		return 0, 0, false, err
	}
	return committed.MessageIDs[0], committed.MessageIDs[1], true, nil
}

func seedCompletionSession(t *testing.T, s *Store, db *sql.DB, project int64) int64 {
	t.Helper()
	record, err := s.CreateSession(t.Context(), project, "model", "", map[string]any{"manager_id": "test-manager"})
	require.NoError(t, err)
	return record.ID
}

func testStore(db *sql.DB) *Store { return NewStore(db) }
func sDB(s *Store) *sql.DB        { return s.db }
func inputFixture(ctx context.Context, s *Store, id int64) (*InboxInput, error) {
	in := &InboxInput{ID: id}
	err := sDB(
		s,
	).QueryRowContext(ctx, "SELECT session_id,source,raw_content FROM session_inbox WHERE id = ?", id).
		Scan(&in.SessionID, &in.Source, &in.RawContent)
	return in, err
}

func newTestStore(t *testing.T) (*Store, *sql.DB, int64) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	res, err := db.ExecContext(
		context.Background(),
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		t.TempDir(), "test",
	)
	require.NoError(t, err)
	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	return testStore(db), db, projectID
}

func assertInputState(t *testing.T, db *sql.DB, id int64, want InputState) {
	t.Helper()
	var state InputState
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT state FROM session_inbox WHERE id = ?", id).Scan(&state))
	require.Equal(t, want, state)
}

func internalTurn(ctx context.Context, s *Store,
	id int64,
	key, fingerprint string,
	assistant, result *transcript.Message,
) (int64, int64, bool, error) {
	enqueued, err := s.Enqueue(
		ctx,
		Input{SessionID: id, Source: InputSourceProcess, Content: result.Content, DeliveryKey: key},
	)
	if err != nil {
		return 0, 0, false, err
	}
	if !enqueued.Applied {
		return 0, 0, false, nil
	}
	committed, err := s.Commit(
		ctx,
		Commit{
			SessionID: id,
			Messages:  []*transcript.Message{assistant, result},
			Accept:    []Accept{{InputID: enqueued.Input.ID, State: InputStateAccepted, LinkRef: 1, ModelBound: true}},
		},
	)
	if err != nil {
		return 0, 0, false, err
	}
	return committed.MessageIDs[0], committed.MessageIDs[1], true, nil
}
