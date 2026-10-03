package configapply

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

var _ Service = (*svc)(nil)

type Service interface {
	Ops() configops.Service
	PendingCall(sessionID int64) (configops.Pending, error)
	ClaimApply() bool
	ReleaseApply()
	Apply(staged *configops.Staged, pending configops.Pending) configops.Verdict
	Restart() <-chan struct{}
	RequestRestart()
	Calls(sessionID int64) map[string]string
	Has(sessionID int64) bool
	RunStagedApply(ctx context.Context, sessionID int64)
	Abandon(ctx context.Context, sessionID int64)
	SettleStagedResults(ctx context.Context, sessionID int64) error
	ConsumeConfigEditActivation(ctx context.Context, sessionID int64, callID string)
}

type stagedCall struct {
	apply  *configops.Staged
	result string
}

type svc struct {
	ops      configops.Service
	sessions *sessionstore.Store
	restart  chan struct{}
	mu       sync.Mutex
	claimed  bool
	calls    map[int64]map[string]stagedCall
}

func New(ops configops.Service, sessions *sessionstore.Store) Service {
	return &svc{ops: ops, sessions: sessions, restart: make(chan struct{}, 1), calls: make(map[int64]map[string]stagedCall)}
}

func (a *svc) Ops() configops.Service { return a.ops }

func (a *svc) PendingCall(sessionID int64) (configops.Pending, error) {
	p, err := a.ops.LoadPending()
	if err != nil {
		return configops.Pending{}, fmt.Errorf("load pending-apply marker: %w", err)
	}
	if p == nil || p.SessionID != sessionID {
		return configops.Pending{}, nil
	}
	return *p, nil
}

func (a *svc) ClaimApply() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.claimed {
		return false
	}
	a.claimed = true
	return true
}

func (a *svc) ReleaseApply() { a.mu.Lock(); defer a.mu.Unlock(); a.claimed = false }

func (a *svc) Apply(staged *configops.Staged, p configops.Pending) configops.Verdict {
	if v := a.ops.Commit(staged, p); v.Failed() {
		a.ReleaseApply()
		return v
	}
	a.RequestRestart()
	return configops.OK()
}

func (a *svc) Restart() <-chan struct{} { return a.restart }

func (a *svc) RequestRestart() {
	select {
	case a.restart <- struct{}{}:
	default:
	}
}

func (a *svc) Calls(sessionID int64) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	calls := make(map[string]string, len(a.calls[sessionID]))
	for id := range a.calls[sessionID] {
		calls[id] = tool.IDConfigEdit
	}
	return calls
}

func (a *svc) Has(sessionID int64) bool { return len(a.Calls(sessionID)) != 0 }

func (a *svc) RunStagedApply(ctx context.Context, sessionID int64) {
	callID, sc, ok := a.takePendingApply(sessionID)
	if !ok {
		return
	}
	if !a.sessions.CallPending(ctx, sessionID, callID) {
		a.ReleaseApply()
		a.resolve(sessionID, callID)
		return
	}
	rec, err := a.sessions.GetSession(ctx, sessionID)
	if err != nil {
		a.ReleaseApply()
		a.deliverResult(ctx, sessionID, callID, "Config change abandoned — the suspend could not be verified, so nothing was written.")
		return
	}
	activation, err := a.sessions.PendingActivation(ctx, sessionID)
	if err != nil || activation.ToolID != tool.IDConfigEdit || activation.Command != ConfigEditCommand ||
		(activation.ToolCallID != "" && activation.ToolCallID != callID) ||
		rec.KilledAt != nil || rec.Status == sessionstore.SessionStatusStopping ||
		rec.Status == sessionstore.SessionStatusStopped || rec.Status == sessionstore.SessionStatusTerminating ||
		rec.Status == sessionstore.SessionStatusKilled {
		a.ReleaseApply()
		a.deliverResult(ctx, sessionID, callID, "Config change abandoned — the session ended before it was applied. Nothing was written.")
		return
	}
	v := a.Apply(sc.apply, configops.Pending{SessionID: sessionID, ToolCallID: callID, ToolName: tool.IDConfigEdit})
	if !v.Failed() {
		a.ConsumeConfigEditActivation(ctx, sessionID, callID)
		return
	}
	a.deliverResult(ctx, sessionID, callID, "Config change rejected — "+v.Reason())
}

func (a *svc) Abandon(ctx context.Context, sessionID int64) {
	callID, _, ok := a.takePendingApply(sessionID)
	if !ok {
		return
	}
	a.ReleaseApply()
	a.stageResult(sessionID, callID, "Config change abandoned — the session ended before it was applied. Nothing was written.")
}

func (a *svc) SettleStagedResults(ctx context.Context, sessionID int64) error {
	a.mu.Lock()
	results := make(map[string]string)
	for id, sc := range a.calls[sessionID] {
		if sc.result != "" {
			results[id] = sc.result
		}
	}
	a.mu.Unlock()
	for id, content := range results {
		rec, err := a.sessions.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		if rec.KilledAt != nil || rec.Status == sessionstore.SessionStatusKilled || rec.Status == sessionstore.SessionStatusTerminating {
			a.resolve(sessionID, id)
			continue
		}
		activation, err := a.sessions.PendingActivation(ctx, sessionID)
		if err != nil && !errors.Is(err, sessionstore.ErrActivationNotFound) {
			return fmt.Errorf("read abandoned activation: %w", err)
		}
		if err == nil && activation.ToolID == tool.IDConfigEdit &&
			(activation.ToolCallID == "" || activation.ToolCallID == id) {
			if _, err := a.sessions.Commit(ctx, sessionstore.Commit{
				SessionID: sessionID, Mode: sessionstore.CommitLifecycle,
				Activation: &sessionstore.ActivationChange{InputID: activation.InputID, State: sessionstore.ActivationExpired},
			}); err != nil {
				return fmt.Errorf("expire abandoned activation: %w", err)
			}
		}
		if !a.sessions.CallPending(ctx, sessionID, id) {
			a.resolve(sessionID, id)
			continue
		}
		if rec.Status == sessionstore.SessionStatusStopping || rec.Status == sessionstore.SessionStatusStopped || rec.Status == sessionstore.SessionStatusKilled {
			_, err = a.sessions.Commit(ctx, sessionstore.Commit{SessionID: sessionID, Mode: sessionstore.CommitLifecycle,
				ToolResults: []*transcript.Message{{Role: "tool", ToolCallID: id, ToolName: tool.IDConfigEdit, Content: content}},
			})
		} else {
			_, err = a.sessions.Enqueue(ctx, sessionstore.Input{SessionID: sessionID, Source: sessionstore.InputSourceCallResult,
				Content: content, Attributes: map[string]any{"call_id": id, "tool_id": tool.IDConfigEdit}, DeliveryKey: "config:" + id,
			})
		}
		if err != nil {
			return err
		}
		a.resolve(sessionID, id)
	}
	return nil
}

func (a *svc) ConsumeConfigEditActivation(ctx context.Context, sessionID int64, callID string) {
	activation, err := a.sessions.CurrentActivation(ctx, sessionID)
	if err != nil {
		if !errors.Is(err, sessionstore.ErrActivationNotFound) {
			logger.Ctx(ctx).Warn("read_config_edit_activation", zap.Error(err))
		}
		return
	}
	if activation.ToolID != tool.IDConfigEdit || activation.Command != ConfigEditCommand {
		return
	}
	err = a.sessions.ConsumeActivationBinding(ctx, sessionstore.ActivationBinding{
		InputID: activation.InputID, SessionID: sessionID, ToolID: activation.ToolID, Command: activation.Command, ToolCallID: callID,
	})
	if err != nil {
		logger.Ctx(ctx).Warn("consume_config_edit_activation", zap.Error(err))
	}
}

func (a *svc) stageApply(sessionID int64, callID string, staged *configops.Staged) bool {
	if !a.ClaimApply() {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.calls[sessionID] == nil {
		a.calls[sessionID] = make(map[string]stagedCall)
	}
	a.calls[sessionID][callID] = stagedCall{apply: staged}
	return true
}

func (a *svc) takePendingApply(sessionID int64) (string, stagedCall, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, sc := range a.calls[sessionID] {
		if sc.apply == nil {
			continue
		}
		taken := sc
		sc.apply = nil
		a.calls[sessionID][id] = sc
		return id, taken, true
	}
	return "", stagedCall{}, false
}

func (a *svc) resolve(sessionID int64, callID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.calls[sessionID], callID)
	if len(a.calls[sessionID]) == 0 {
		delete(a.calls, sessionID)
	}
}

func (a *svc) stageResult(sessionID int64, callID, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls[sessionID][callID] = stagedCall{result: content}
}

func (a *svc) deliverResult(ctx context.Context, sessionID int64, callID, content string) {
	a.stageResult(sessionID, callID, content)
	if err := a.SettleStagedResults(ctx, sessionID); err != nil {
		logger.Ctx(ctx).Named("configapply").Error("result_delivery_failed", zap.Int64("session_id", sessionID), zap.Error(err))
	}
}
