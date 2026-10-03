package sessionprompt

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/loader"
)

// PreparedMessage contains transcript text and the canonical activated skill name for its receipt.
type PreparedMessage struct {
	Content   string
	SkillName string
}

// PrepareUserMessageDetailed expands leading skill commands and preserves ordinary messages.
func PrepareUserMessageDetailed(ldr loader.Registry, message string) (PreparedMessage, error) {
	expanded, skillName, matched, err := expandSkillCommand(ldr, message)
	if !matched {
		return PreparedMessage{Content: message}, nil
	}

	return PreparedMessage{Content: expanded, SkillName: skillName}, err
}

func expandSkillCommand(ldr loader.Registry, message string) (string, string, bool, error) {
	name, args, matched, err := parseSkillCommand(message)
	if !matched || err != nil {
		return message, "", matched, err
	}

	if ldr == nil {
		return "", "", true, errors.New("no skills are available")
	}

	sk := ldr.GetSkill(name)
	if sk == nil || !sk.IsUserInvocable() {
		skills := ldr.ListUserInvocableSkills()
		names := make([]string, len(skills))

		for i, available := range skills {
			names[i] = available.Name
		}

		return "", "", true, fmt.Errorf("skill unavailable: %s\nAvailable skills: %v", name, names)
	}

	return loader.RenderSkillInvocation(sk, args), sk.Name, true, nil
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
