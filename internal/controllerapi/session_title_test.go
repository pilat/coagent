package controllerapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSessionTitle(t *testing.T) {
	for _, tt := range []struct {
		name, raw, normalized, derived string
		valid                          bool
	}{
		{"trim", "  fix login  ", "fix login", "fix login", true},
		{"empty", "", "", "", false},
		{"whitespace", " \t\n", "", "", false},
		{"80 runes", strings.Repeat("界", 80), strings.Repeat("界", 80), strings.Repeat("界", 80), true},
		{"81 runes", strings.Repeat("界", 81), strings.Repeat("界", 81), strings.Repeat("界", 79) + "…", false},
		{"LF", "first\nsecond", "first\nsecond", "first", false},
		{"CRLF", "first\r\nsecond", "first\r\nsecond", "first", false},
		{"CR", "first\rsecond", "first\rsecond", "first", false},
		{"command", " /skill foo", "/skill foo", "", true},
		{"leading blank", "\n \t\nfirst\nsecond", "first\nsecond", "first", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := NormalizeSessionTitle(tt.raw)
			assert.Equal(t, tt.normalized, got)
			assert.Equal(t, tt.valid, ok)
			derived := SessionTitleFromInput(tt.raw)
			assert.Equal(t, tt.derived, derived)
			if derived != "" {
				_, valid := NormalizeSessionTitle(derived)
				assert.True(t, valid)
			}
		})
	}
}
