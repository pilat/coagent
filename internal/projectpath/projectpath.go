package projectpath

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

const (
	defaultRoot         = "~/" + coagenthome.DirName + "/" + coagenthome.ProjectsDirName
	maxProjectNameRunes = 64
)

func ResolveRoot(unified *config.UnifiedConfig) string {
	root := defaultRoot
	if unified != nil && strings.TrimSpace(unified.ProjectsRoot) != "" {
		root = strings.TrimSpace(unified.ProjectsRoot)
	}

	if strings.HasPrefix(root, "~/") {
		if home, err := coagenthome.UserHome(); err == nil {
			root = filepath.Join(home, root[2:])
		}
	}

	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}

	return filepath.Clean(root)
}

func Same(left, right string) bool {
	if left == "" || right == "" {
		return false
	}

	leftAbs, leftErr := filepath.Abs(left)

	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}

	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func SanitizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)

	if name == "" {
		return "", errors.New("project name is empty")
	}

	if strings.ContainsAny(name, "/\\:\x00") {
		return "", errors.New(`project name must not contain "/", "\", ":", or a NUL byte`)
	}

	if name == controllerapi.CoagentSystemProjectDir {
		return "", errors.New("project name is reserved")
	}

	if strings.HasPrefix(name, ".") {
		return "", errors.New(`project name must not start with "."`)
	}

	if utf8.RuneCountInString(name) > maxProjectNameRunes {
		return "", fmt.Errorf("project name must be at most %d characters", maxProjectNameRunes)
	}

	return name, nil
}
