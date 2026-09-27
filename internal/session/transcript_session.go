package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
)

var _ TranscriptSession = (*transcriptSession)(nil)

// TranscriptSession settles durable calls without constructing executable tools.
type TranscriptSession interface {
	PendingExternalCalls() []PendingToolCall
	ResolvePendingCall(context.Context, PendingToolCall, string) (CallResolution, error)
	SettleStoppedCalls(context.Context, string) error
	ResolveInterruptedCalls(context.Context, []PendingToolCall, string) error
	ReloadDeliveredCompletion(context.Context) error
	HasPendingWork() bool
}

type transcriptSession struct {
	ms          *messageStore
	stagedCalls map[string]string
}

// OpenTranscript loads the call-settlement projection without runtime resources.
func OpenTranscript(
	ctx context.Context,
	store sessionstore.RuntimeStore,
	outputStore sessionstore.RuntimeOutputStore,
	sessionID int64,
	stagedCalls map[string]string,
) (TranscriptSession, error) {
	if store == nil || sessionID == 0 {
		return nil, errors.New("transcript store and session ID are required")
	}

	ms := newMessageStore(store, sessionID, outputStore)
	if err := ms.reloadMessages(ctx); err != nil {
		return nil, fmt.Errorf("load transcript: %w", err)
	}

	return &transcriptSession{ms: ms, stagedCalls: stagedCalls}, nil
}

func (s *transcriptSession) ReloadDeliveredCompletion(ctx context.Context) error {
	return s.ms.reloadMessages(ctx)
}

func (s *svc) PendingExternalCalls() []PendingToolCall {
	return s.transcript().PendingExternalCalls()
}

func (s *svc) ResolvePendingCall(ctx context.Context, call PendingToolCall, content string) (CallResolution, error) {
	return s.transcript().ResolvePendingCall(ctx, call, content)
}

func (s *svc) SettleStoppedCalls(ctx context.Context, content string) error {
	return s.transcript().SettleStoppedCalls(ctx, content)
}

func (s *svc) ResolveInterruptedCalls(ctx context.Context, calls []PendingToolCall, content string) error {
	return s.transcript().ResolveInterruptedCalls(ctx, calls, content)
}

func (s *svc) HasPendingWork() bool {
	return s.transcript().HasPendingWork()
}

func (s *svc) transcript() *transcriptSession {
	return &transcriptSession{ms: s.ms, stagedCalls: s.stagedCalls}
}
