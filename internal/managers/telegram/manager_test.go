package telegram

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTextPreview(t *testing.T) {
	assert.Empty(t, textPreview(""))
	assert.Equal(t, "short message", textPreview("short message"))

	// 60 ASCII runes → truncated to 48 + ellipsis (49 runes).
	long := textPreview(strings.Repeat("a", 60))
	assert.Len(t, []rune(long), 49)
	assert.True(t, strings.HasSuffix(long, "…"))

	// Multibyte must truncate on a rune boundary, never split a rune.
	cyr := textPreview(strings.Repeat("я", 60))
	assert.Len(t, []rune(cyr), 49)
	assert.True(t, strings.HasSuffix(cyr, "…"))
	assert.True(t, strings.HasPrefix(cyr, "я"))
}
