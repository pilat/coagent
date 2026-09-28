package session

import "sync"

type checkpointTerminal struct {
	phase   string
	content string
	outcome checkpointResult
}

type checkpointAttempt struct {
	input    *PendingInput
	focus    string
	terminal *checkpointTerminal
}

type checkpointControl struct {
	mu          sync.Mutex
	pending     bool
	queuedInput *PendingInput
	queuedFocus string
	active      *checkpointAttempt
}

func (c *checkpointControl) request() {
	c.mu.Lock()
	c.pending = true
	c.mu.Unlock()
}

func (c *checkpointControl) queue(input PendingInput, focus string) {
	c.mu.Lock()
	c.queuedInput = &input
	c.queuedFocus = focus
	c.pending = true
	c.mu.Unlock()
}

func (c *checkpointControl) requested() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.pending || c.active != nil
}

// claim keeps the active command separate from requests that arrive mid-attempt.
func (c *checkpointControl) claim() *checkpointAttempt {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.active != nil {
		return c.active
	}

	if !c.pending {
		return nil
	}

	c.active = &checkpointAttempt{input: c.queuedInput, focus: c.queuedFocus}
	c.pending = false
	c.queuedInput = nil
	c.queuedFocus = ""

	return c.active
}

func (c *checkpointControl) focus() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.active != nil {
		return c.active.focus
	}

	return ""
}

func (c *checkpointControl) finish(attempt *checkpointAttempt) {
	if attempt == nil {
		return
	}

	c.mu.Lock()
	if c.active == attempt {
		c.active = nil
	}
	c.mu.Unlock()
}
