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
	apply  *configops.Staged
	result string
}

// After restart, ownership is reconstructed from durable producer ledgers;
// this map contains only the current process's outstanding work and adoption.
type stagedCalls struct {
	mu        sync.Mutex
	bySession map[int64]map[string]stagedCall
}

func newStagedCalls() *stagedCalls {
	return &stagedCalls{bySession: make(map[int64]map[string]stagedCall)}
}

func (c *stagedCalls) stage(sessionID int64, callID, toolName string) {
	c.put(sessionID, callID, stagedCall{toolName: toolName})
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

func (c *stagedCalls) stageResult(sessionID int64, callID, toolName, content string) {
	c.put(sessionID, callID, stagedCall{toolName: toolName, result: content})
}

func (c *stagedCalls) pendingResults(sessionID int64) map[string]stagedCall {
	c.mu.Lock()
	defer c.mu.Unlock()

	results := make(map[string]stagedCall)

	for callID, sc := range c.bySession[sessionID] {
		if sc.result != "" {
			results[callID] = sc
		}
	}

	return results
}

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
