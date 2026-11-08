package builtin

import "github.com/pilat/coagent/internal/tool"

const browserFrameResultLimit = 120000

var _ tool.FrameRule = browserFrameRule{}

type browserFrameRule struct{}

// Frames returns the stack's frame rule independently of which servers are up.
func (s *Stack) Frames() tool.FrameRule {
	return browserFrameRule{}
}

func (browserFrameRule) Owns(id string) bool {
	return tool.IsBrowserToolID(id)
}

func (browserFrameRule) ResultLimit() int {
	return browserFrameResultLimit
}
