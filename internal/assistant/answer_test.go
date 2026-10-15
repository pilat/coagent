package assistant

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validAnswer = `{"say":null}`

func TestAssistantParse(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		bad  string
	}{
		{"plain", validAnswer, ""},
		{"empty object", `{}`, ""},
		{"fenced", "```json\n" + validAnswer + "\n```", ""},
		{"prose", "Here is the answer: " + validAnswer + " trailing prose", ""},
		{"braces in strings", `{"say":"a } \\\" { b"}`, ""},
		{"edits", `{"focus":"f","stickers":{"a":"t","b":{"text":"t","on":244},"c":null},"diary":{"k":"v","x":null}}`, ""},
		{"unknown field", `{"extra":1,"say":null}`, "unknown field"},
		{"think is gone", `{"think":"hm","say":null}`, "unknown field"},
		{"rewritten stickers", `{"stickers":["whole list"]}`, "cannot unmarshal"},
		{"sticker unknown", `{"stickers":{"a":{"text":"t","x":1}}}`, "unknown field"},
		{"sticker bad on", `{"stickers":{"a":{"text":"t","on":true}}}`, "session id or a project name"},
		{"bad say", `{"say":4}`, "cannot unmarshal"},
		{"nested unknown", `{"say":null,"do":[{"stop":{"session":1,"extra":1}}]}`, "unknown field"},
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
			assert.Empty(t, Validate(answer, Desk{}.Edited(answer), DefaultRules(), []Card{{ID: 244}}))
		})
	}
}

func TestAssistantParseStickerEdits(t *testing.T) {
	t.Parallel()
	a, err := Parse(`{"stickers":{"plain":"text","id":{"text":"t","on":244},"name":{"text":"t","on":"@devbox"},
		"quoted":{"text":"t","on":"245"},"peel":null}}`)
	require.NoError(t, err)
	assert.Equal(t, &StickerEdit{Text: "text"}, a.Stickers["plain"])
	assert.Equal(t, &StickerEdit{Text: "t", On: "244"}, a.Stickers["id"])
	assert.Equal(t, &StickerEdit{Text: "t", On: "devbox"}, a.Stickers["name"])
	assert.Equal(t, &StickerEdit{Text: "t", On: "245"}, a.Stickers["quoted"])
	peel, exists := a.Stickers["peel"]
	assert.True(t, exists)
	assert.Nil(t, peel)
}

func TestAssistantDeskEdited(t *testing.T) {
	t.Parallel()
	before := Desk{
		Diary:    map[string]string{"a": "1", "b": "2"},
		Stickers: []Sticker{{Key: "k1", Text: "t1", On: "244"}, {Key: "k2", Text: "t2"}},
		Focus:    "f",
	}
	a, err := Parse(`{"diary":{"a":"x","b":null,"c":"new","gone":null},
		"stickers":{"k1":"t1b","k2":null,"k4":{"text":"t4"},"k3":{"text":"t3","on":"devbox"},"never":null}}`)
	require.NoError(t, err)

	after := before.Edited(a)
	assert.Equal(t, map[string]string{"a": "x", "c": "new"}, after.Diary)
	assert.Equal(t, []Sticker{
		{Key: "k1", Text: "t1b", On: "244"},
		{Key: "k3", Text: "t3", On: "devbox"},
		{Key: "k4", Text: "t4"},
	}, after.Stickers, "a rewrite keeps the binding; new keys append in key order")
	assert.Equal(t, "f", after.Focus, "an absent focus stays")
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, before.Diary, "the loaded desk is not aliased")
	assert.Equal(t, "t1", before.Stickers[0].Text)

	focus := ""
	assert.Empty(t, before.Edited(Answer{Focus: &focus}).Focus, "a written empty focus clears it")
}

func TestAssistantValidate(t *testing.T) {
	text := func(s string) *string { return &s }
	for _, tc := range []struct {
		name   string
		change func(*Answer)
		bad    string
	}{
		{"null say", func(*Answer) {}, ""},
		{"ninth sticker", func(a *Answer) {
			for i := range 9 {
				a.Stickers[fmt.Sprint(i)] = &StickerEdit{Text: "t"}
			}
		}, "too many stickers"},
		{"sticker chars", func(a *Answer) { a.Stickers["k"] = &StickerEdit{Text: strings.Repeat("я", 1001)} }, "sticker character"},
		{"sticker text", func(a *Answer) { a.Stickers["k"] = &StickerEdit{Text: " "} }, "a sticker needs text"},
		{"sticker key", func(a *Answer) { a.Stickers["one\ntwo"] = &StickerEdit{Text: "t"} }, "sticker keys"},
		{"sticker on board", func(a *Answer) { a.Stickers["k"] = &StickerEdit{Text: "t", On: "244"} }, ""},
		{"sticker on unknown", func(a *Answer) { a.Stickers["k"] = &StickerEdit{Text: "t", On: "42"} }, "neither a board"},
		{"sticker on opened", func(a *Answer) {
			a.Do = []Action{{Open: &OpenAction{Project: "devbox", Text: "go"}}}
			a.Stickers["k"] = &StickerEdit{Text: "t", On: "devbox"}
		}, ""},
		{"sticker on unopened", func(a *Answer) { a.Stickers["k"] = &StickerEdit{Text: "t", On: "devbox"} }, "neither a board"},
		{"diary chars", func(a *Answer) { a.Diary["k"] = text(strings.Repeat("я", 2000)) }, "diary character"},
		{"diary boundary", func(a *Answer) { a.Diary["k"] = text(strings.Repeat("я", 1999)) }, ""},
		{"diary key", func(a *Answer) { a.Diary[strings.Repeat("я", 65)] = text("v") }, "diary keys"},
		{"diary text", func(a *Answer) { a.Diary["k"] = text("") }, "a diary entry needs text"},
		{"diary entries", func(a *Answer) {
			for i := range 31 {
				a.Diary[string(rune('а'+i))] = text("v")
			}
		}, "too many diary"},
		{"focus chars", func(a *Answer) { a.Focus = text(strings.Repeat("я", 201)) }, "focus character"},
		{"focus line", func(a *Answer) { a.Focus = text("one\ntwo") }, "one line"},
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
			a := Answer{Stickers: map[string]*StickerEdit{}, Diary: map[string]*string{}}
			tc.change(&a)
			violations := Validate(a, Desk{}.Edited(a), DefaultRules(), []Card{{ID: 244}})
			if tc.bad == "" {
				assert.Empty(t, violations)
				return
			}

			assert.Contains(t, violationsText(violations), tc.bad)
		})
	}
}

func TestAssistantValidateCountsTheEditedDesk(t *testing.T) {
	t.Parallel()
	full := Desk{Diary: map[string]string{}}
	for i := range 8 {
		full.Stickers = append(full.Stickers, Sticker{Key: fmt.Sprint(i), Text: "t"})
	}

	add := Answer{Stickers: map[string]*StickerEdit{"new": {Text: "t"}}}
	assert.Contains(t, violationsText(Validate(add, full.Edited(add), DefaultRules(), nil)), "too many stickers")

	swap := Answer{Stickers: map[string]*StickerEdit{"new": {Text: "t"}, "0": nil}}
	assert.Empty(t, Validate(swap, full.Edited(swap), DefaultRules(), nil), "peeling one makes room")
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
	a, err := Parse("{\"say\":\"line one\nline two\ttab\"}")
	require.NoError(t, err)
	require.NotNil(t, a.Say)
	assert.Equal(t, "line one\nline two\ttab", *a.Say)
}
