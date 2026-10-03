package session

import (
	"fmt"

	"github.com/pilat/coagent/internal/sessionprompt"
)

func (s *Session) PrepareUserMessage(message string) (string, error) {
	prepared, err := s.PrepareUserMessageDetailed(message)
	return prepared.Content, err
}

func (s *Session) PrepareUserMessageDetailed(message string) (sessionprompt.PreparedMessage, error) {
	prepared, err := sessionprompt.PrepareUserMessageDetailed(s.loader, message)
	if err != nil {
		return prepared, fmt.Errorf("prepare user message: %w", err)
	}

	return prepared, nil
}
