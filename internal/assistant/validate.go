package assistant

import (
	"fmt"
	"slices"
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

// Validate applies the same authority and capacity checks on every re-ask.
func Validate(a Answer, rules Rules, board []Card, _ []Line) []Violation {
	var violations []Violation
	check := func(bad bool, message string) {
		if bad {
			violations = append(violations, Violation(message))
		}
	}
	check(len(a.Diary) > rules.DiaryEntries, "too many diary entries")
	check(diaryChars(a.Diary) > rules.DiaryChars, "diary character limit exceeded")

	for key := range a.Diary {
		check(utf8.RuneCountInString(key) > rules.KeyChars, "diary key character limit exceeded")
	}

	check(len(a.Stickers) > rules.Stickers, "too many stickers")
	check(utf8.RuneCountInString(strings.Join(a.Stickers, "")) > rules.StickerChars, "sticker character limit exceeded")
	check(utf8.RuneCountInString(a.Focus) > rules.FocusChars, "focus character limit exceeded")
	check(strings.ContainsAny(a.Focus, "\r\n"), "focus must be one line")
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

func diaryChars(diary map[string]string) int {
	var count int
	for key, value := range diary {
		count += utf8.RuneCountInString(key) + utf8.RuneCountInString(value)
	}

	return count
}
