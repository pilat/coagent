package assistant

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Rules are host limits, never model-controlled settings.
type Rules struct {
	DiaryEntries int
	DiaryChars   int
	KeyChars     int
	Stickers     int
	StickerChars int
	FocusChars   int
	Actions      int
}

type Violation string

// DefaultRules defines the fixed desk capacity.
func DefaultRules() Rules {
	return Rules{
		DiaryEntries: 30, DiaryChars: 2000, KeyChars: 64,
		Stickers: 8, StickerChars: 1000, FocusChars: 200, Actions: 5,
	}
}

// Validate checks the answer and the desk it leaves behind, the same way on every re-ask.
func Validate(a Answer, next Desk, rules Rules, board []Card) []Violation {
	var violations []Violation
	check := func(bad bool, message string) {
		if bad {
			violations = append(violations, Violation(message))
		}
	}
	check(len(next.Diary) > rules.DiaryEntries, "too many diary entries: erase spent ones with null")
	check(diaryChars(next.Diary) > rules.DiaryChars, "diary character limit exceeded")

	for _, key := range slices.Sorted(maps.Keys(a.Diary)) {
		entry := a.Diary[key]
		check(!validKey(key, rules), fmt.Sprintf("diary keys must be one line of 1-%d characters", rules.KeyChars))
		check(entry != nil && strings.TrimSpace(*entry) == "", "a diary entry needs text; null erases it")
	}

	check(len(next.Stickers) > rules.Stickers, "too many stickers: peel spent ones with null")
	check(stickerChars(next.Stickers) > rules.StickerChars, "sticker character limit exceeded")

	for _, key := range slices.Sorted(maps.Keys(a.Stickers)) {
		edit := a.Stickers[key]
		check(!validKey(key, rules), fmt.Sprintf("sticker keys must be one line of 1-%d characters", rules.KeyChars))
		check(edit != nil && strings.TrimSpace(edit.Text) == "", "a sticker needs text; null peels it")

		if edit != nil && edit.On != "" && !bindable(edit.On, a, board) {
			violations = append(violations, Violation(fmt.Sprintf(
				"sticker %s: on %q is neither a board session nor a project opened in this answer", key, edit.On)))
		}
	}

	check(utf8.RuneCountInString(next.Focus) > rules.FocusChars, "focus character limit exceeded")
	check(strings.ContainsAny(next.Focus, "\r\n"), "focus must be one line")
	check(len(a.Do) > rules.Actions, "too many actions")

	for _, action := range a.Do {
		violations = append(violations, validateAction(action, board)...)
	}

	for _, span := range a.Forget {
		check(len(span) != 2 || span[0] <= 0 || span[0] > span[1], "forget requires an inclusive [from,to] range")
	}

	return violations
}

func validateAction(a Action, board []Card) []Violation {
	var count int
	var sessionID int64
	var violations []Violation

	if a.Open != nil {
		count++

		if strings.TrimSpace(a.Open.Project) == "" || strings.TrimSpace(a.Open.Text) == "" {
			violations = append(violations, "open requires project and text")
		}
	}

	if a.Send != nil {
		count++

		sessionID = a.Send.Session
		if strings.TrimSpace(a.Send.Text) == "" {
			violations = append(violations, "send requires text")
		}
	}

	if a.Read != nil {
		count++

		sessionID = a.Read.Session
		if a.Read.Lines < 1 || a.Read.Lines > 60 {
			violations = append(violations, "read.lines must be between 1 and 60")
		}
	}

	if a.Stop != nil {
		count++
		sessionID = a.Stop.Session
	}

	if count != 1 {
		return append(violations, "each action must contain exactly one operation")
	}

	if a.Open == nil && !slices.ContainsFunc(board, func(card Card) bool { return card.ID == sessionID }) {
		violations = append(violations, Violation(fmt.Sprintf("session %d is not on the board", sessionID)))
	}

	return violations
}

func validKey(key string, rules Rules) bool {
	return strings.TrimSpace(key) != "" && utf8.RuneCountInString(key) <= rules.KeyChars &&
		!strings.ContainsAny(key, "\r\n")
}

func bindable(on string, a Answer, board []Card) bool {
	if id, err := strconv.ParseInt(on, 10, 64); err == nil {
		return slices.ContainsFunc(board, func(card Card) bool { return card.ID == id })
	}

	return slices.ContainsFunc(a.Do, func(action Action) bool {
		return action.Open != nil && action.Open.Project == on
	})
}

func diaryChars(diary map[string]string) int {
	var count int
	for key, value := range diary {
		count += utf8.RuneCountInString(key) + utf8.RuneCountInString(value)
	}

	return count
}
