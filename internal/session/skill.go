package session

import "github.com/pilat/coagent/internal/sessionprompt"

func (s *Session) PrepareUserMessage(message string) (string, error) {
	prepared, err := s.PrepareUserMessageDetailed(message)
	return prepared.Content, err
}

func (s *Session) PrepareUserMessageDetailed(message string) (sessionprompt.PreparedMessage, error) {
	return sessionprompt.PrepareUserMessageDetailed(s.loader, message)
}
