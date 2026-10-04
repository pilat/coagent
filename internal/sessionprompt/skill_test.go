package sessionprompt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSkillCommand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		message     string
		wantName    string
		wantArgs    string
		wantMatched bool
		wantError   bool
	}{
		{name: "simple", message: "/skill review", wantName: "review", wantMatched: true},
		{name: "leading whitespace", message: " \t/skill review", wantName: "review", wantMatched: true},
		{name: "newline separator", message: "/skill\nreview", wantName: "review", wantMatched: true},
		{
			name:        "multiline arguments",
			message:     "/skill  review  first\n  second  ",
			wantName:    "review",
			wantArgs:    "first\n  second",
			wantMatched: true,
		},
		{name: "missing name", message: "/skill", wantMatched: true, wantError: true},
		{name: "whitespace only name", message: "/skill   ", wantMatched: true, wantError: true},
		{name: "lookalike", message: "/skillful review"},
		{name: "colon command", message: "/skill:review"},
		{name: "later occurrence", message: "please /skill review"},
		{name: "other command", message: "/status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, args, matched, err := parseSkillCommand(tc.message)

			assert.Equal(t, tc.wantName, name)
			assert.Equal(t, tc.wantArgs, args)
			assert.Equal(t, tc.wantMatched, matched)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
