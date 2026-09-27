package sandboxpolicy

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
)

// ValidateSection rejects malformed rules and canonical project aliases.
func ValidateSection(section Section) error {
	for i, rule := range section.Rules {
		if err := validateRule(rule); err != nil {
			return fmt.Errorf("sandbox.rules %d: %w", i, err)
		}
	}

	keys := make([]string, 0, len(section.Projects))
	for key := range section.Projects {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	seen := make(map[string]string, len(keys))

	for _, key := range keys {
		placeholder, err := CanonicalProjectKey(key)
		if err != nil {
			return fmt.Errorf("sandbox.projects %q: %w", key, err)
		}

		if other, ok := seen[placeholder]; ok {
			return fmt.Errorf("sandbox.projects %q and %q name the same project", other, key)
		}

		seen[placeholder] = key

		for i, rule := range section.Projects[key].Rules {
			if err := validateRule(rule); err != nil {
				return fmt.Errorf("sandbox.projects %q rules %d: %w", key, i, err)
			}
		}
	}

	return nil
}

// SelectProjectRules returns the rules that apply to a project: the entry whose
// canonical key equals canonicalRoot, plus — when sourceRepoRoot is non-empty —
// the entry for that source repository, because a worktree coagent created from
// a project inherits that project's rules. Source-repository rules come first,
// then the project's own, so the more specific ones win.
func SelectProjectRules(section Section, canonicalRoot, sourceRepoRoot string) ([]Rule, error) {
	if err := ValidateSection(section); err != nil {
		return nil, err
	}

	rules := make([]Rule, 0)

	if sourceRepoRoot != "" {
		sourceRules, err := lookupProjectRules(section, sourceRepoRoot)
		if err != nil {
			return nil, err
		}

		rules = append(rules, sourceRules...)
	}

	ownRules, err := lookupProjectRules(section, canonicalRoot)
	if err != nil {
		return nil, err
	}

	return append(rules, ownRules...), nil
}

func lookupProjectRules(section Section, canonicalRoot string) ([]Rule, error) {
	canonicalRoot, err := CanonicalProjectKey(canonicalRoot)
	if err != nil {
		return nil, err
	}

	for key, project := range section.Projects {
		resolved, err := CanonicalProjectKey(key)
		if err != nil {
			return nil, fmt.Errorf("sandbox.projects %q: %w", key, err)
		}

		if resolved == canonicalRoot {
			return project.Rules, nil
		}
	}

	return nil, nil
}

func validateRule(rule Rule) error {
	action, path, err := ruleActionPath(rule)
	if err != nil {
		return err
	}

	if action == ActionDeny && rule.Mode != "" {
		return errors.New("mode is only valid on an allow rule")
	}

	if rule.Mode != "" && rule.Mode != ModeReadOnly && rule.Mode != ModeReadWrite {
		return fmt.Errorf("mode %q must be %q or %q", rule.Mode, ModeReadOnly, ModeReadWrite)
	}

	return validateRulePath(path)
}

func validateRulePath(path string) error {
	placeholder, err := placeholderPath(path)
	if err != nil {
		return err
	}

	if !filepath.IsAbs(placeholder) {
		return fmt.Errorf("path %q must be absolute or start with ~/", path)
	}

	return rejectDotDot(path)
}
