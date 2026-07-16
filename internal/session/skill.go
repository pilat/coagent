package session

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/tool/builtin"
)

// PrepareUserMessage expands a leading skill command and leaves ordinary messages unchanged.
func (s *svc) PrepareUserMessage(message string) (string, error) {
	expanded, matched, err := s.expandSkillCommand(message)
	if !matched {
		return message, nil
	}

	return expanded, err
}

func (s *svc) expandSkillCommand(message string) (string, bool, error) {
	name, args, matched, err := parseSkillCommand(message)
	if !matched || err != nil {
		return message, matched, err
	}

	if s.loader == nil {
		return "", true, errors.New("no skills are available")
	}

	sk := s.loader.GetSkill(name)
	if sk == nil || !sk.IsUserInvocable() {
		skills := s.loader.ListUserInvocableSkills()
		names := make([]string, len(skills))

		for i, available := range skills {
			names[i] = available.Name
		}

		return "", true, fmt.Errorf("skill unavailable: %s\nAvailable skills: %v", name, names)
	}

	return builtin.RenderSkill(sk, args), true, nil
}

func parseSkillCommand(message string) (string, string, bool, error) {
	const command = "/skill"

	input := strings.TrimLeftFunc(message, unicode.IsSpace)
	if !strings.HasPrefix(input, command) {
		return "", "", false, nil
	}

	rest := input[len(command):]
	if rest != "" {
		separator, _ := utf8.DecodeRuneInString(rest)
		if !unicode.IsSpace(separator) {
			return "", "", false, nil
		}
	}

	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)

	if rest == "" {
		return "", "", true, errors.New("skill name is required")
	}

	nameEnd := strings.IndexFunc(rest, unicode.IsSpace)
	if nameEnd < 0 {
		return rest, "", true, nil
	}

	name := rest[:nameEnd]
	args := strings.TrimSpace(rest[nameEnd:])

	return name, args, true, nil
}
