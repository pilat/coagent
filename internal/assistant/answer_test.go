package assistant

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validAnswer = `{"say":null,"focus":"","stickers":[],"diary":{}}`

func TestAssistantParse(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		bad  string
	}{
		{"plain", validAnswer, ""},
		{"fenced", "```json\n" + validAnswer + "\n```", ""},
		{"prose", "Here is the answer: " + validAnswer + " trailing prose", ""},
		{"braces in strings", `{"say":"a } \\\" { b","focus":"","stickers":[],"diary":{}}`, ""},
		{"unknown field", strings.Replace(validAnswer, `"focus"`, `"extra":1,"focus"`, 1), "unknown field"},
		{"missing diary", `{"say":null,"focus":"","stickers":[]}`, "diary"},
		{"missing say", `{"focus":"","stickers":[],"diary":{}}`, "say"},
		{"null slots", `{"say":null,"focus":"","stickers":null,"diary":{}}`, "stickers"},
		{"bad say", `{"say":4,"focus":"","stickers":[],"diary":{}}`, "cannot unmarshal"},
		{"nested unknown", `{"say":null,"focus":"","stickers":[],"diary":{},"do":[{"stop":{"session":1,"extra":1}}]}`, "unknown field"},
		{"unfinished", `{"say":`, "unfinished"},
		{"absent", "no answer", "no JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answer, err := Parse(tc.text)
			if tc.bad != "" {
				require.ErrorContains(t, err, tc.bad)
				return
			}

			require.NoError(t, err)
			assert.Empty(t, Validate(answer, DefaultRules(), nil, nil))
		})
	}
}

func TestAssistantValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Answer)
		bad    string
	}{
		{"null say", func(*Answer) {}, ""},
		{"ninth sticker", func(a *Answer) { a.Stickers = make([]string, 9) }, "too many stickers"},
		{"sticker chars", func(a *Answer) { a.Stickers = []string{strings.Repeat("я", 1001)} }, "sticker character"},
		{"diary chars", func(a *Answer) { a.Diary = map[string]string{"k": strings.Repeat("я", 2000)} }, "diary character"},
		{"diary boundary", func(a *Answer) { a.Diary = map[string]string{"k": strings.Repeat("я", 1999)} }, ""},
		{"diary key", func(a *Answer) { a.Diary = map[string]string{strings.Repeat("я", 65): "v"} }, "diary key"},
		{"diary entries", func(a *Answer) {
			for i := range 31 {
				a.Diary[string(rune('а'+i))] = "v"
			}
		}, "too many diary"},
		{"focus chars", func(a *Answer) { a.Focus = strings.Repeat("я", 201) }, "focus character"},
		{"focus line", func(a *Answer) { a.Focus = "one\ntwo" }, "one line"},
		{"unknown session", func(a *Answer) { a.Do = []Action{{Send: &SendAction{Session: 42, Text: "go"}}} }, "not on the board"},
		{"known session", func(a *Answer) { a.Do = []Action{{Send: &SendAction{Session: 244, Text: "go"}}} }, ""},
		{"read zero", func(a *Answer) { a.Do = []Action{{Read: &ReadAction{Session: 244}}} }, "read.lines"},
		{"read 61", func(a *Answer) { a.Do = []Action{{Read: &ReadAction{Session: 244, Lines: 61}}} }, "read.lines"},
		{"read 60", func(a *Answer) { a.Do = []Action{{Read: &ReadAction{Session: 244, Lines: 60}}} }, ""},
		{"forget event", func(a *Answer) { a.Forget = [][]int64{{30, 34}} }, ""},
		{"forget old", func(a *Answer) { a.Forget = [][]int64{{1, 33}} }, ""},
		{"forget arity", func(a *Answer) { a.Forget = [][]int64{{1, 2, 3}} }, "[from,to]"},
		{"forget order", func(a *Answer) { a.Forget = [][]int64{{3, 1}} }, "[from,to]"},
		{"empty action", func(a *Answer) { a.Do = []Action{{}} }, "exactly one"},
		{"multiple operations", func(a *Answer) {
			a.Do = []Action{{Stop: &StopAction{Session: 244}, Read: &ReadAction{Session: 244, Lines: 1}}}
		}, "exactly one"},
		{"six actions", func(a *Answer) { a.Do = make([]Action, 6) }, "too many actions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := Parse(validAnswer)
			require.NoError(t, err)
			tc.change(&a)
			violations := Validate(a, DefaultRules(), []Card{{ID: 244}}, nil)
			if tc.bad == "" {
				assert.Empty(t, violations)
				return
			}

			assert.Contains(t, violationsText(violations), tc.bad)
		})
	}
}

func TestAssistantThinkTruncated(t *testing.T) {
	t.Parallel()
	text, err := json.Marshal(map[string]any{
		"think": strings.Repeat("я", 401), "say": nil,
		"focus": "", "stickers": []string{}, "diary": map[string]string{},
	})
	require.NoError(t, err)
	a, err := Parse(string(text))
	require.NoError(t, err)
	assert.Equal(t, 400, utf8.RuneCountInString(a.Think))
	assert.Empty(t, Validate(a, DefaultRules(), nil, nil))
	assert.Nil(t, a.Say)
}

func violationsText(violations []Violation) string {
	var parts []string
	for _, violation := range violations {
		parts = append(parts, string(violation))
	}

	return strings.Join(parts, "; ")
}

func TestAssistantParseRawLineBreaks(t *testing.T) {
	t.Parallel()
	a, err := Parse("{\"say\":\"line one\nline two\ttab\",\"focus\":\"\",\"stickers\":[],\"diary\":{}}")
	require.NoError(t, err)
	require.NotNil(t, a.Say)
	assert.Equal(t, "line one\nline two\ttab", *a.Say)
}
