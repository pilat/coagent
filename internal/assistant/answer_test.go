package assistant

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validAnswer = "{\"say\":null}"

func TestAssistantParse(t *testing.T) {
	for _, tc := range []struct{ name, text, bad string }{
		{"plain", validAnswer, ""},
		{"empty object", "{}", ""},
		{"fenced", "```json\n" + validAnswer + "\n```", ""},
		{"prose", "Here is the answer: " + validAnswer + " trailing prose", ""},
		{"braces in strings", `{"say":"a } \\\" { b"}`, ""},
		{"edits", `{"notes":[{"key":"user.lang","text":"Russian"},{"key":"open.old","text":null}],"focus":"f"}`, ""},
		{"unknown field", `{"extra":1}`, "unknown field"},
		{"think is gone", `{"think":"hm"}`, "unknown field"},
		{"diary is gone", `{"diary":{"k":"v"}}`, "unknown field"},
		{"stickers are gone", `{"stickers":{}}`, "unknown field"},
		{"map notes", `{"notes":{"user.lang":"Russian"}}`, "cannot unmarshal"},
		{"object note", `{"notes":[{"key":"user.lang","text":{"text":"v"}}]}`, "cannot unmarshal"},
		{"number note", `{"notes":[{"key":"user.lang","text":5}]}`, "cannot unmarshal"},
		{"unknown note field", `{"notes":[{"key":"user.lang","text":"v","extra":1}]}`, "unknown field"},
		{"duplicate note", `{"notes":[{"key":"user.lang","text":"v"},{"key":"user.lang","text":null}]}`, "note user.lang is written twice in one answer"},
		{"bad say", `{"say":4}`, "cannot unmarshal"},
		{"nested unknown", `{"do":[{"stop":{"session":1,"extra":1}}]}`, "unknown field"},
		{"unfinished", `{"say":`, "unfinished"},
		{"absent", "no answer", "no JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.text)
			if tc.bad != "" {
				require.ErrorContains(t, err, tc.bad)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAssistantParseNotesAndOpen(t *testing.T) {
	t.Parallel()
	a, err := Parse(
		`{"notes":[{"key":"user.lang","text":"Russian"},{"key":"open.old","text":null}],"do":[{"open":{"project":"devbox","text":"go","about":"why"}}]}`,
	)
	require.NoError(t, err)
	require.NotNil(t, a.Notes["user.lang"])
	assert.Equal(t, "Russian", *a.Notes["user.lang"])
	text, exists := a.Notes["open.old"]
	assert.True(t, exists)
	assert.Nil(t, text)
	assert.Equal(t, "why", a.Do[0].Open.About)
}

func TestAssistantDeskEdited(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	before := Desk{
		Notes: []Note{
			{Key: "open.a", Text: "1", WrittenAt: 10},
			{Key: "open.b", Text: "2", WrittenAt: 20},
			{Key: "rule.keep", Text: "safe", WrittenAt: 30},
		},
		Focus: "f",
	}
	a, err := Parse(
		`{"notes":[{"key":"open.a","text":"x"},{"key":"open.b","text":null},{"key":"open.c","text":"new"},{"key":"open.gone","text":null}]}`,
	)
	require.NoError(t, err)
	after := before.Edited(a, now)
	assert.Equal(
		t,
		[]Note{
			{Key: "open.a", Text: "x", WrittenAt: 1000},
			{Key: "open.c", Text: "new", WrittenAt: 1000},
			{Key: "rule.keep", Text: "safe", WrittenAt: 30},
		},
		after.Notes,
	)
	assert.Equal(t, "f", after.Focus)
	assert.Equal(t, "1", before.Notes[0].Text)
	empty := ""
	assert.Empty(t, before.Edited(Answer{Focus: &empty}, now).Focus)
}

func validationFixture() ValidationContext {
	projects := []Project{{Key: "devbox", Sessions: []Card{{ID: 244}}, SessionIDs: []int64{244}}}
	shown := TrimCatalog(projects, Desk{}, Event{}, time.Unix(1000, 0))
	return ValidationContext{
		OriginKind:   "user",
		Shown:        shown,
		ProjectKeys:  shown.ProjectKeys,
		ExistingKeys: map[string]bool{"wait.999": true, "project.gone": true},
	}
}

func TestAssistantValidate(t *testing.T) {
	text := func(s string) *string { return &s }
	for _, tc := range []struct {
		name   string
		change func(*Answer)
		bad    string
	}{
		{"null say", func(*Answer) {}, ""},
		{"user boundary", func(a *Answer) { a.Notes["user.pref"] = text(strings.Repeat("я", 1500)) }, ""},
		{"user and rule combined", func(a *Answer) {
			a.Notes["user.pref"] = text(strings.Repeat("я", 1000))
			a.Notes["rule.git"] = text(strings.Repeat("я", 501))
		}, "exceed 1500"},
		{"open boundary", func(a *Answer) { a.Notes["open.todo"] = text(strings.Repeat("я", 800)) }, ""},
		{"open over", func(a *Answer) { a.Notes["open.todo"] = text(strings.Repeat("я", 801)) }, "exceed 800"},
		{"project boundary", func(a *Answer) { a.Notes["project.devbox"] = text(strings.Repeat("я", 300)) }, ""},
		{"project over", func(a *Answer) { a.Notes["project.devbox"] = text(strings.Repeat("я", 301)) }, "exceed 300"},
		{"session boundary", func(a *Answer) { a.Notes["session.244"] = text(strings.Repeat("я", 300)) }, ""},
		{"session over", func(a *Answer) { a.Notes["session.244"] = text(strings.Repeat("я", 301)) }, "exceed 300"},
		{"wait boundary", func(a *Answer) { a.Notes["wait.244"] = text(strings.Repeat("я", 300)) }, ""},
		{"wait over", func(a *Answer) { a.Notes["wait.244"] = text(strings.Repeat("я", 301)) }, "exceed 300"},
		{"empty note", func(a *Answer) { a.Notes["open.a"] = text(" ") }, "needs text"},
		{"unknown scope", func(a *Answer) { a.Notes["diary.a"] = text("x") }, "valid target"},
		{"empty suffix", func(a *Answer) { a.Notes["user."] = text("x") }, "valid target"},
		{"suffix boundary", func(a *Answer) { a.Notes["user."+strings.Repeat("я", 64)] = text("x") }, ""},
		{"suffix over", func(a *Answer) { a.Notes["user."+strings.Repeat("я", 65)] = text("x") }, "valid target"},
		{"multiline key", func(a *Answer) { a.Notes["project.devbox\nx"] = text("x") }, "valid target"},
		{"unknown project", func(a *Answer) { a.Notes["project.unknown"] = text("x") }, "neither in the catalog"},
		{"unknown session", func(a *Answer) { a.Notes["session.42"] = text("x") }, "neither in the catalog"},
		{"unknown wait", func(a *Answer) { a.Notes["wait.42"] = text("x") }, "neither in the catalog"},
		{"orphan deletion", func(a *Answer) { a.Notes["wait.999"] = nil; a.Notes["project.gone"] = nil }, ""},
		{"opened project", func(a *Answer) {
			a.Do = []Action{{Open: &OpenAction{Project: "new", Text: "go", About: "why"}}}
			a.Notes["wait.new"] = text("retell")
			a.Notes["project.new"] = text("purpose")
		}, ""},
		{"existing project open", func(a *Answer) {
			a.Do = []Action{{Open: &OpenAction{Project: "devbox", Text: "go", About: "why"}}}
			a.Notes["wait.devbox"] = text("retell")
			a.Notes["project.devbox"] = text("purpose")
		}, ""},
		{"raw session forbidden", func(a *Answer) {
			a.Do = []Action{{Open: &OpenAction{Project: "new", Text: "go", About: "why"}}}
			a.Notes["wait.new"] = text("retell")
			a.Notes["session.new"] = text("wrong")
		}, "neither in the catalog"},
		{"duplicate open", func(a *Answer) {
			a.Do = []Action{{Open: &OpenAction{Project: "devbox", Text: "a", About: "a"}}, {Open: &OpenAction{Project: "devbox", Text: "b", About: "b"}}}
			a.Notes["wait.devbox"] = text("retell")
		}, "one open per project"},
		{"open without wait", func(a *Answer) { a.Do = []Action{{Open: &OpenAction{Project: "devbox", Text: "go", About: "why"}}} }, "needs a wait note"},
		{"open without about", func(a *Answer) {
			a.Do = []Action{{Open: &OpenAction{Project: "devbox", Text: "go"}}}
			a.Notes["wait.devbox"] = text("retell")
		}, "open.about"},
		{"about boundary", func(a *Answer) {
			a.Do = []Action{{Open: &OpenAction{Project: "devbox", Text: "go", About: strings.Repeat("я", 200)}}}
			a.Notes["wait.devbox"] = text("retell")
		}, ""},
		{"about over", func(a *Answer) {
			a.Do = []Action{{Open: &OpenAction{Project: "devbox", Text: "go", About: strings.Repeat("я", 201)}}}
			a.Notes["wait.devbox"] = text("retell")
		}, "open.about"},
		{"about multiline", func(a *Answer) {
			a.Do = []Action{{Open: &OpenAction{Project: "devbox", Text: "go", About: "one\ntwo"}}}
			a.Notes["wait.devbox"] = text("retell")
		}, "open.about"},
		{"focus chars", func(a *Answer) { a.Focus = text(strings.Repeat("я", 201)) }, "focus character"},
		{"focus line", func(a *Answer) { a.Focus = text("one\ntwo") }, "one line"},
		{"unknown action target", func(a *Answer) {
			a.Do = []Action{{Send: &SendAction{Session: 42, Text: "go"}}}
			a.Notes["wait.42"] = text("retell")
		}, "session 42 is not in the catalog"},
		{"send paired", func(a *Answer) {
			a.Do = []Action{{Send: &SendAction{Session: 244, Text: "go"}}}
			a.Notes["wait.244"] = text("retell")
		}, ""},
		{"send unpaired", func(a *Answer) { a.Do = []Action{{Send: &SendAction{Session: 244, Text: "go"}}} }, "needs a wait note"},
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
			a := Answer{Notes: map[string]*string{}}
			tc.change(&a)
			violations := Validate(a, Desk{}.Edited(a, time.Unix(1000, 0)), DefaultRules(), validationFixture())
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
	full := Desk{}
	ctx := validationFixture()
	for i := range 8 {
		full.Notes = append(full.Notes, Note{Key: fmt.Sprintf("wait.%d", i+1), Text: "t"})
		ctx.Shown.Cards = append(ctx.Shown.Cards, Card{ID: int64(i + 1)})
	}
	add := Answer{Notes: map[string]*string{"wait.244": new("new")}}
	assert.Contains(
		t,
		violationsText(Validate(add, full.Edited(add, time.Now()), DefaultRules(), ctx)),
		"exceed 8 entries",
	)
	swap := Answer{Notes: map[string]*string{"wait.244": new("new"), "wait.1": nil}}
	assert.Empty(t, Validate(swap, full.Edited(swap, time.Now()), DefaultRules(), ctx))
	existing := Answer{Do: []Action{{Send: &SendAction{Session: 244, Text: "go"}}}}
	assert.Empty(t, Validate(existing, Desk{Notes: []Note{{Key: "wait.244", Text: "pending"}}}, DefaultRules(), ctx))
}

func TestAssistantPinnedOriginAndFullProjectKeys(t *testing.T) {
	t.Parallel()
	ctx := validationFixture()
	ctx.OriginKind = "observe"
	ctx.ProjectKeys["~/"+strings.Repeat("long/", 20)+"repo"] = true
	a := Answer{Notes: map[string]*string{"user.pref": new("short")}}
	assert.Contains(
		t,
		violationsText(Validate(a, Desk{}.Edited(a, time.Now()), DefaultRules(), ctx)),
		"starts with the user's message",
	)
	a.Notes = map[string]*string{"user.pref": nil}
	assert.Empty(t, Validate(a, Desk{}, DefaultRules(), ctx))
	a.Notes = map[string]*string{"project.~ /wrong": new("x")}
	assert.NotEmpty(t, Validate(a, Desk{}.Edited(a, time.Now()), DefaultRules(), ctx))
	for key := range ctx.ProjectKeys {
		a.Notes = map[string]*string{"project." + key: new("state")}
		assert.Empty(t, Validate(a, Desk{}.Edited(a, time.Now()), DefaultRules(), ctx))
	}
}

func violationsText(violations []Violation) string {
	var parts []string
	for _, v := range violations {
		parts = append(parts, string(v))
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
