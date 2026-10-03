package daemon

import (
	"context"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/progressruntime"
)

func runnerWaitingCount(runners *runnerSet) int {
	runners.mu.Lock()
	defer runners.mu.Unlock()

	return len(runners.waiting)
}

func runnerLiveCount(runners *runnerSet) int {
	runners.mu.Lock()
	defer runners.mu.Unlock()

	return len(runners.byID)
}

func runnerCount(runners *runnerSet) int {
	return runnerLiveCount(runners)
}

func runnerCounts(runners *runnerSet) (int, int) {
	runners.mu.Lock()
	defer runners.mu.Unlock()

	return runners.running, runners.children
}

func runnerRunningCount(runners *runnerSet) int {
	runners.mu.Lock()
	defer runners.mu.Unlock()

	return runners.running
}

func runnerChildCount(runners *runnerSet) int {
	runners.mu.Lock()
	defer runners.mu.Unlock()

	return runners.children
}

func registerTestRunner(ctx context.Context, manager *svc, rs *runner) (*runner, bool) {
	if !manager.runners.tryAdmit(rs.child, rs.parentID) {
		return nil, false
	}

	manager.liveMu.Lock()
	defer manager.liveMu.Unlock()

	manager.progress.SetLive(rs.sessionID, progressruntime.Live{Active: true})
	existing, registered := manager.runners.register(rs)
	if !registered {
		manager.runners.release(rs.child, rs.parentID)
		manager.updateLiveLocked(ctx, rs.sessionID)
	}

	return existing, registered
}

func transcriptOf(h *subagentHarness, sessionID int64) []llmwire.Message {
	messages, err := h.sessStore.LoadActiveMessages(h.ctx, sessionID)
	if err != nil {
		h.t.Fatalf("load transcript for session %d: %v", sessionID, err)
	}
	return toDTO(messages)
}
