package sessionstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

var fixtureDBs sync.Map

func enqueueInput(ctx context.Context, s *sessionstore.Store,
	id int64,
	source sessionstore.InputSource,
	content string,
) (*sessionstore.InboxInput, error) {
	result, err := s.Enqueue(ctx, sessionstore.Input{SessionID: id, Source: source, Content: content})
	if err != nil || result == nil {
		return nil, err
	}
	return result.Input, nil
}

func appendMessages(ctx context.Context, s *sessionstore.Store,
	id int64,
	messages []*transcript.Message,
) ([]int64, error) {
	result, err := s.Commit(ctx, sessionstore.Commit{SessionID: id, Messages: messages})
	if err != nil {
		return nil, err
	}
	return result.MessageIDs, nil
}

func appendMessage(ctx context.Context, s *sessionstore.Store, id int64, message *transcript.Message) (int64, error) {
	ids, err := appendMessages(ctx, s, id, []*transcript.Message{message})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

func acceptInput(ctx context.Context, s *sessionstore.Store,
	inputID int64,
	content string,
) (*transcript.Message, error) {
	in, err := inputFixture(ctx, s, inputID)
	if err != nil {
		return nil, err
	}
	result, err := s.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: in.SessionID,
			Accept: []sessionstore.Accept{
				{
					InputID:    inputID,
					State:      sessionstore.InputStateAccepted,
					Content:    content,
					LinkRef:    -1,
					ModelBound: true,
				},
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

func stepState(ctx context.Context, s *sessionstore.Store,
	id int64,
	iteration int,
	status sessionstore.SessionStatus,
) error {
	_, err := s.Commit(
		ctx,
		sessionstore.Commit{SessionID: id, State: sessionstore.StatePatch{Iteration: &iteration, Status: &status}},
	)
	return err
}

func createChild(ctx context.Context, s *sessionstore.Store,
	project, parent, root int64,
	agent, model, reasoning string,
) (int64, error) {
	var id int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = sessionstore.CreateSubagentSessionTx(
			ctx,
			tx,
			sessionstore.CreateSubagentSession{
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

func replaceTranscript(ctx context.Context, s *sessionstore.Store,
	id int64,
	head []int64,
	entries []sessionstore.CompactionEntry,
) ([]int64, error) {
	_, err := s.Commit(
		ctx,
		sessionstore.Commit{SessionID: id, Replace: &sessionstore.Replace{HeadIDs: head, Entries: entries}},
	)
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

func appendPublished(ctx context.Context, s *sessionstore.Store,
	id int64,
	message *transcript.Message,
	kind sessionstore.OutputType,
	content string,
	releases bool,
) (int64, *sessionstore.OutputCommit, error) {
	result, err := s.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: id,
			Messages:  []*transcript.Message{message},
			Outputs: []sessionstore.Output{
				{Type: kind, Content: content, ReleasesInput: releases, MessageRef: 0, Phase: "reply"},
			},
		},
	)
	if err != nil {
		return 0, nil, err
	}
	var output *sessionstore.OutputCommit
	if len(result.Outputs) > 0 {
		output = result.Outputs[0]
	}
	return result.MessageIDs[0], output, nil
}

func answerTool(ctx context.Context, s *sessionstore.Store,
	id int64,
	message *transcript.Message,
	outputs []string,
) (int64, []*sessionstore.OutputCommit, error) {
	var parts []sessionstore.Output
	for i, draft := range outputs {
		parts = append(
			parts,
			sessionstore.Output{
				Type:          sessionstore.OutputMessagePersistent,
				Content:       draft,
				Key:           fmt.Sprintf("tool:%s:direct:%d", message.ToolCallID, i),
				ReleasesInput: true,
			},
		)
	}
	result, err := s.Commit(
		ctx,
		sessionstore.Commit{SessionID: id, ToolResults: []*transcript.Message{message}, Outputs: parts},
	)
	if err != nil {
		return 0, nil, err
	}
	return result.MessageIDs[0], result.Outputs, nil
}

func scheduledTurn(ctx context.Context, s *sessionstore.Store,
	id int64,
	key, fingerprint string,
	assistant, result *transcript.Message,
) (int64, int64, bool, error) {
	enqueued, err := s.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:   id,
			Source:      sessionstore.InputSourceSchedule,
			Content:     result.Content,
			DeliveryKey: key,
		},
	)
	if err != nil {
		return 0, 0, false, err
	}
	if !enqueued.Applied {
		return 0, 0, false, nil
	}
	committed, err := s.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: id,
			Messages:  []*transcript.Message{assistant, result},
			Outputs: []sessionstore.Output{
				{
					Type:       sessionstore.OutputMessagePersistent,
					Content:    result.Content,
					Key:        "schedule:" + key + ":announcement",
					Attributes: map[string]any{"source": "scheduler"},
				},
			},
			Accept: []sessionstore.Accept{
				{InputID: enqueued.Input.ID, State: sessionstore.InputStateAccepted, LinkRef: 1, ModelBound: true},
			},
		},
	)
	if err != nil {
		return 0, 0, false, err
	}
	return committed.MessageIDs[0], committed.MessageIDs[1], true, nil
}

func freshTurn(ctx context.Context, s *sessionstore.Store,
	id int64,
	key, fingerprint string,
	messages []*transcript.Message,
) ([]int64, bool, error) {
	enqueued, err := s.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:   id,
			Source:      sessionstore.InputSourceSchedule,
			Content:     "fresh scheduled task",
			DeliveryKey: key,
			Attributes:  map[string]any{"fresh": true},
		},
	)
	if err != nil {
		return nil, false, err
	}
	if !enqueued.Applied {
		return nil, false, nil
	}
	committed, err := s.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: id,
			Messages:  messages,
			Accept: []sessionstore.Accept{
				{InputID: enqueued.Input.ID, State: sessionstore.InputStateAccepted, LinkRef: 0, ModelBound: true},
			},
			State: sessionstore.StatePatch{ResetContext: true},
		},
	)
	if err != nil {
		return nil, false, err
	}
	return committed.MessageIDs, true, nil
}

func seedCompletionSession(t *testing.T, s *sessionstore.Store, db *sql.DB, project int64) int64 {
	t.Helper()
	record, err := s.CreateSession(t.Context(), project, "model", "", map[string]any{"manager_id": "test-manager"})
	require.NoError(t, err)
	return record.ID
}

func assistantStopMessage(content string) *transcript.Message {
	return &transcript.Message{Role: "assistant", Content: content, FinishType: "stop"}
}

func testStore(db *sql.DB) *sessionstore.Store {
	s := sessionstore.NewStore(db)
	fixtureDBs.Store(s, db)
	return s
}
func sDB(s *sessionstore.Store) *sql.DB { db, _ := fixtureDBs.Load(s); return db.(*sql.DB) }
func inputFixture(ctx context.Context, s *sessionstore.Store, id int64) (*sessionstore.InboxInput, error) {
	in := &sessionstore.InboxInput{ID: id}
	err := sDB(
		s,
	).QueryRowContext(ctx, "SELECT session_id,source,raw_content FROM session_inbox WHERE id = ?", id).
		Scan(&in.SessionID, &in.Source, &in.RawContent)
	return in, err
}

func deliverChild(ctx context.Context, s *sessionstore.Store,
	parent int64,
	messages []*transcript.Message,
	child, seq int64,
) ([]int64, bool, error) {
	link, err := subagent.NewStore(sDB(s)).GetLink(ctx, child)
	if err != nil {
		return nil, false, err
	}
	producer := subagent.NewTransactions(sDB(s), s)
	if !link.Terminal() && seq == link.ActivationSeq {
		_, err = producer.TryFinalizeActivation(
			ctx,
			child,
			subagent.StateCompleted,
			messages[len(messages)-1].Content,
			subagent.OutcomeCompleted,
		)
		if err != nil {
			return nil, false, err
		}
		link, err = subagent.NewStore(sDB(s)).GetLink(ctx, child)
		if err != nil {
			return nil, false, err
		}
	}
	link.ParentID = parent
	link.ActivationSeq = seq
	won, err := producer.DeliverBackgroundCompletion(ctx, *link, 0)
	if err != nil || !won {
		return nil, won, err
	}
	input, err := s.PeekPending(ctx, parent)
	if err != nil {
		return nil, false, err
	}
	prepared := make([]*transcript.Message, len(messages))
	for i, msg := range messages {
		stored := *msg
		prepared[i] = &stored
	}
	prepared[len(prepared)-1].Content = input.RawContent
	committed, err := s.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: parent,
			Messages:  prepared,
			Accept: []sessionstore.Accept{
				{
					InputID:    input.ID,
					State:      sessionstore.InputStateAccepted,
					LinkRef:    len(prepared) - 1,
					ModelBound: true,
				},
			},
		},
	)
	if err != nil {
		return nil, false, err
	}
	return committed.MessageIDs, true, nil
}

func releaseFixtureDB(db *sql.DB) {
	fixtureDBs.Range(func(key, value any) bool {
		if value == db {
			fixtureDBs.Delete(key)
		}
		return true
	})
}
