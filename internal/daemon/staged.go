package daemon

import (
	"sync"

	"github.com/pilat/coagent/internal/configops"
)

// stagedCall is one tool call the daemon owes a result for.
type stagedCall struct {
	toolName string
	// apply is handed over exactly once, then nil; nil too for calls whose
	// outside work is not a config write.
	apply *configops.Staged
}

// stagedCalls is the in-flight ledger of calls the daemon owes a result for.
// In-memory on purpose: a daemon that died before recording the work never did
// it, so re-executing the call is then correct.
type stagedCalls struct {
	mu        sync.Mutex
	bySession map[int64]map[string]stagedCall
}

func newStagedCalls() *stagedCalls {
	return &stagedCalls{bySession: make(map[int64]map[string]stagedCall)}
}

// stage records that sessionID's callID is out with the world.
func (c *stagedCalls) stage(sessionID int64, callID, toolName string) {
	c.put(sessionID, callID, stagedCall{toolName: toolName})
}

// stageApply reserves the daemon-wide apply slot and records the call the verdict
// is owed to. A refusal reaches the tool before it suspends, never after.
func (s *svc) stageApply(sessionID int64, callID, toolName string, staged *configops.Staged) bool {
	if s.applier == nil || !s.applier.ClaimApply() {
		return false
	}

	s.staged.put(sessionID, callID, stagedCall{toolName: toolName, apply: staged})

	return true
}

// takePendingApply hands over a session's staged config change, exactly once.
// The call itself stays registered until its verdict is injected.
func (c *stagedCalls) takePendingApply(sessionID int64) (string, stagedCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for callID, sc := range c.bySession[sessionID] {
		if sc.apply == nil {
			continue
		}

		taken := sc
		sc.apply = nil
		c.bySession[sessionID][callID] = sc

		return callID, taken, true
	}

	return "", stagedCall{}, false
}

// resolve forgets a call once its result has been injected.
func (c *stagedCalls) resolve(sessionID int64, callID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	calls := c.bySession[sessionID]
	if calls == nil {
		return
	}

	delete(calls, callID)

	if len(calls) == 0 {
		delete(c.bySession, sessionID)
	}
}

// forSession lists a session's staged calls (id → tool name) for the session
// constructor.
func (c *stagedCalls) forSession(sessionID int64) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.bySession[sessionID]) == 0 {
		return nil
	}

	out := make(map[string]string, len(c.bySession[sessionID]))
	for callID, sc := range c.bySession[sessionID] {
		out[callID] = sc.toolName
	}

	return out
}

func (c *stagedCalls) has(sessionID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.bySession[sessionID]) > 0
}

func (c *stagedCalls) put(sessionID int64, callID string, sc stagedCall) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.bySession[sessionID] == nil {
		c.bySession[sessionID] = make(map[string]stagedCall)
	}

	c.bySession[sessionID][callID] = sc
}
