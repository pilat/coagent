package assistant

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	noteScopeUser    = "user"
	noteScopeRule    = "rule"
	noteScopeOpen    = "open"
	noteScopeProject = "project"
	noteScopeSession = "session"
	noteScopeWait    = "wait"
)

// Rules are host limits, never model-controlled settings.
type Rules struct {
	UserChars  int
	OpenChars  int
	BoundChars int
	Waits      int
	KeyChars   int
	FocusChars int
	Actions    int
}

type Violation string

// ValidationContext preserves the origin's authority across read continuations.
type ValidationContext struct {
	OriginKind   string
	Shown        ShownCatalog
	ProjectKeys  map[string]bool
	ExistingKeys map[string]bool
}

// DefaultRules defines the fixed desk capacity.
func DefaultRules() Rules {
	return Rules{UserChars: 1500, OpenChars: 800, BoundChars: 300, Waits: 8, KeyChars: 64, FocusChars: 200, Actions: 5}
}

// Validate checks both the edits and the resulting desk before any effect.
func Validate(a Answer, next Desk, rules Rules, ctx ValidationContext) []Violation {
	var violations []Violation
	check := func(bad bool, message string) {
		if bad {
			violations = append(violations, Violation(message))
		}
	}

	violations = append(violations, validateNotes(a, next, rules, ctx)...)
	check(utf8.RuneCountInString(next.Focus) > rules.FocusChars, "focus character limit exceeded")
	check(strings.ContainsAny(next.Focus, "\r\n"), "focus must be one line")
	check(len(a.Do) > rules.Actions, "too many actions")

	opened := map[string]bool{}

	for _, action := range a.Do {
		violations = append(violations, validateAction(action, ctx.Shown.Cards)...)
		if action.Open != nil {
			check(opened[action.Open.Project], "one open per project per answer")
			opened[action.Open.Project] = true
		}
	}

	notes := noteMap(next.Notes)

	for _, action := range a.Do {
		paired := true

		switch {
		case action.Open != nil:
			paired = a.Notes["wait."+action.Open.Project] != nil
		case action.Send != nil:
			_, paired = notes["wait."+strconv.FormatInt(action.Send.Session, 10)]
		}

		check(!paired, "every open and send needs a wait note saying what to tell the user when it answers")
	}

	for _, span := range a.Forget {
		check(len(span) != 2 || span[0] <= 0 || span[0] > span[1], "forget requires an inclusive [from,to] range")
	}

	return violations
}

func validateNotes(a Answer, next Desk, rules Rules, ctx ValidationContext) []Violation {
	var violations []Violation

	for _, key := range slices.Sorted(maps.Keys(a.Notes)) {
		text := a.Notes[key]

		scope, target, ok := strings.Cut(key, ".")
		if !ok || !validNoteKey(scope, target, rules) {
			violations = append(
				violations,
				Violation("note "+key+": use user., rule., open., project., session. or wait. with a valid target"),
			)

			continue
		}

		if text == nil && ctx.ExistingKeys[key] {
			continue
		}

		if text != nil && strings.TrimSpace(*text) == "" {
			violations = append(violations, "a note needs text; null deletes it")
		}

		if text != nil && (scope == noteScopeUser || scope == noteScopeRule) && ctx.OriginKind != userKind {
			violations = append(
				violations,
				"user and rule notes are set only on a turn that starts with the user's message",
			)
		}

		if !noteBindable(scope, target, a, ctx) {
			violations = append(
				violations,
				Violation(fmt.Sprintf("note %s: %s is neither in the catalog nor opened in this answer", key, target)),
			)
		}
	}

	return append(violations, validateNoteCapacity(next, rules)...)
}

func validateNoteCapacity(next Desk, rules Rules) []Violation {
	var violations []Violation

	if scopeChars(next.Notes, "user.")+scopeChars(next.Notes, "rule.") > rules.UserChars {
		violations = append(
			violations,
			Violation(
				fmt.Sprintf("user and rule notes exceed %d characters: merge, shorten or delete", rules.UserChars),
			),
		)
	}

	if scopeChars(next.Notes, "open.") > rules.OpenChars {
		violations = append(
			violations,
			Violation(fmt.Sprintf("open notes exceed %d characters: merge, shorten or delete", rules.OpenChars)),
		)
	}

	waits := 0

	for _, note := range next.Notes {
		scope, _, _ := strings.Cut(note.Key, ".")
		if scope == noteScopeWait {
			waits++
		}

		if (scope == noteScopeProject || scope == noteScopeSession || scope == noteScopeWait) &&
			utf8.RuneCountInString(note.Text) > rules.BoundChars {
			violations = append(
				violations,
				Violation(
					fmt.Sprintf(
						"note %s: texts exceed %d characters: merge, shorten or delete",
						note.Key,
						rules.BoundChars,
					),
				),
			)
		}
	}

	if waits > rules.Waits {
		violations = append(
			violations,
			Violation(fmt.Sprintf("wait notes exceed %d entries: merge or delete", rules.Waits)),
		)
	}

	return violations
}

func validNoteKey(scope, target string, rules Rules) bool {
	if strings.TrimSpace(target) == "" || strings.ContainsAny(target, "\r\n") {
		return false
	}

	switch scope {
	case noteScopeUser, noteScopeRule, noteScopeOpen:
		return utf8.RuneCountInString(target) <= rules.KeyChars
	case noteScopeProject, noteScopeSession, noteScopeWait:
		return true
	default:
		return false
	}
}

func noteBindable(scope, target string, a Answer, ctx ValidationContext) bool {
	switch scope {
	case noteScopeUser, noteScopeRule, noteScopeOpen:
		return true
	case noteScopeProject:
		if ctx.ProjectKeys[target] {
			return true
		}

		return openedProject(a, target)
	case noteScopeWait:
		if openedProject(a, target) {
			return true
		}
	}

	id, err := strconv.ParseInt(target, 10, 64)

	return err == nil && id > 0 && strconv.FormatInt(id, 10) == target &&
		slices.ContainsFunc(ctx.Shown.Cards, func(card Card) bool { return card.ID == id })
}

func openedProject(a Answer, target string) bool {
	return slices.ContainsFunc(
		a.Do,
		func(action Action) bool { return action.Open != nil && action.Open.Project == target },
	)
}

func validateAction(a Action, cards []Card) []Violation {
	count := 0
	id := int64(0)
	var violations []Violation

	if a.Open != nil {
		count++

		if strings.TrimSpace(a.Open.Project) == "" || strings.TrimSpace(a.Open.Text) == "" {
			violations = append(violations, "open requires project and text")
		}

		if strings.TrimSpace(a.Open.About) == "" || strings.ContainsAny(a.Open.About, "\r\n") ||
			utf8.RuneCountInString(a.Open.About) > 200 {
			violations = append(violations, "open.about requires one line of 1-200 characters")
		}
	}

	if a.Send != nil {
		count++

		id = a.Send.Session
		if strings.TrimSpace(a.Send.Text) == "" {
			violations = append(violations, "send requires text")
		}
	}

	if a.Read != nil {
		count++

		id = a.Read.Session
		if a.Read.Lines < 1 || a.Read.Lines > 60 {
			violations = append(violations, "read.lines must be between 1 and 60")
		}
	}

	if a.Stop != nil {
		count++
		id = a.Stop.Session
	}

	if count != 1 {
		return append(violations, "each action must contain exactly one operation")
	}

	if a.Open == nil && !slices.ContainsFunc(cards, func(card Card) bool { return card.ID == id }) {
		violations = append(violations, Violation(fmt.Sprintf("session %d is not in the catalog", id)))
	}

	return violations
}
