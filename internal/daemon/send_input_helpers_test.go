package daemon

import "context"

func (s *Service) sendToSession(ctx context.Context, sessionID int64, prompt string) error {
	return s.sendInput(ctx, sessionID, prompt, "")
}
