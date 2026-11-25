package hooks

import (
	"context"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

var exactCharacters = regexp.MustCompile(`^[A-Za-z0-9_ ,|\-]+$`)

type matcher struct {
	all     bool
	exact   []string
	pattern *regexp.Regexp
}

func compileMatcher(ctx context.Context, value, path, event string) matcher {
	if event == stop || value == "" || value == "*" {
		return matcher{all: true}
	}

	if exactCharacters.MatchString(value) {
		return matcher{exact: strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '|' })}
	}

	pattern, err := regexp.Compile(value)
	if err != nil {
		logger.Ctx(ctx).Warn("hooks_matcher_invalid", zap.String("matcher", value), zap.String("path", path))
	}

	return matcher{pattern: pattern}
}

func (m matcher) matches(names []string) bool {
	if m.all {
		return true
	}

	for _, name := range names {
		for _, exact := range m.exact {
			if strings.TrimSpace(exact) == name {
				return true
			}
		}

		if m.pattern != nil && m.pattern.MatchString(name) {
			return true
		}
	}

	return false
}
