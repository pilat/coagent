package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

type externalCallCoordinator interface {
	Pending(context.Context, int64) (map[string]string, error)
	ConfigEditTool(int64) tool.Tool
	StageApply(int64, string, string, *configops.Staged) bool
	Apply(context.Context, int64, func() bool)
	Abandon(context.Context, int64)
	SettleResults(context.Context, int64) error
	Resolve(context.Context, int64, pendingCallResolver, pendingCallResultInput) (bool, error)
	SettleStopped(context.Context, int64) error
	CloseOrphans(context.Context, *sessionstore.SessionRecord) (int, error)
	CloseInterrupted(context.Context, *sessionstore.SessionRecord) (int, error)
	ConsumeActivation(context.Context, int64, string)
	ExpireActivation(context.Context, int64) error
}

var _ externalCallCoordinator = (*externalCalls)(nil)

type externalTranscriptReader interface {
	LoadActiveMessages(context.Context, int64) ([]*transcript.Message, error)
}

type pendingSleepReader interface {
	PendingSleeps(context.Context, int64) ([]schedule.PendingSleep, error)
}

type pendingChildReader interface {
	ListPendingChildLinks(context.Context, int64) ([]subagent.Link, error)
}

type pendingCallResolver interface {
	ResolvePendingCall(context.Context, session.PendingToolCall, string) (session.CallResolution, error)
}

type externalCalls struct {
	staged          *stagedCalls
	applier         configapply.Service
	activationStore sessionstore.ActivationStore
	sessionStore    externalTranscriptReader
	runtimeStore    sessionstore.RuntimeStore
	scheduleSvc     pendingSleepReader
	links           pendingChildReader
	enqueue         func(context.Context, int64, pendingCallResultInput) error
}

func newExternalCalls(
	applier configapply.Service,
	activationStore sessionstore.ActivationStore,
	sessionStore externalTranscriptReader,
	runtimeStore sessionstore.RuntimeStore,
	scheduleSvc pendingSleepReader,
	links pendingChildReader,
	enqueue func(context.Context, int64, pendingCallResultInput) error,
) externalCallCoordinator {
	return &externalCalls{
		staged: newStagedCalls(), applier: applier, activationStore: activationStore,
		sessionStore: sessionStore, runtimeStore: runtimeStore, scheduleSvc: scheduleSvc,
		links: links, enqueue: enqueue,
	}
}

// Resolve retires process ownership only after the transcript accepts the exact result.
func (s *externalCalls) Resolve(
	ctx context.Context,
	sessionID int64,
	resolver pendingCallResolver,
	input pendingCallResultInput,
) (bool, error) {
	resolution, err := resolver.ResolvePendingCall(ctx, input.Call, input.Content)
	if err != nil {
		return false, fmt.Errorf("resolve %s call %s: %w", input.Call.Name, input.Call.ID, err)
	}

	s.staged.resolve(sessionID, input.Call.ID)

	return resolution == session.CallResolutionInserted, nil
}

// StageApply reserves the daemon-wide apply slot and records the call the verdict
// is owed to. A refusal reaches the tool before it suspends, never after.
func (s *externalCalls) StageApply(sessionID int64, callID, toolName string, staged *configops.Staged) bool {
	if s.applier == nil || !s.applier.ClaimApply() {
		return false
	}

	s.staged.put(sessionID, callID, stagedCall{toolName: toolName, apply: staged})

	return true
}
