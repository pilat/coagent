package telegram

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTextToTelegramHTMLHeadings(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"h1 underlined", "# Title", "<b><u>Title</u></b>"},
		{"h2 underlined", "## Title", "<b><u>Title</u></b>"},
		{"h3 bold only", "### Title", "<b>Title</b>"},
		{"h6 bold only", "###### Title", "<b>Title</b>"},
		{"hashtag untouched", "#tag here", "#tag here"},
		{"numeric ref untouched", "fix #5 now", "fix #5 now"},
		{"mid-line hash untouched", "see #Title", "see #Title"},
		{"bold inside heading", "## **B** x", "<b><u><b>B</b> x</u></b>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, textToTelegramHTML(tt.in))
		})
	}
}

func TestTextToTelegramHTMLNarrowTable(t *testing.T) {
	in := "| A | B |\n|---|---|\n| 1 | 2 |"
	want := "<pre>| A | B |\n| 1 | 2 |</pre>"
	assert.Equal(t, want, textToTelegramHTML(in))
}

func TestTextToTelegramHTMLTablePreservesSurroundingText(t *testing.T) {
	in := "before\n\n| A | B |\n| 1 | 2 |\n\nafter"
	want := "before\n\n<pre>| A | B |\n| 1 | 2 |</pre>\n\nafter"
	assert.Equal(t, want, textToTelegramHTML(in))
}

func TestRenderMarkdownTableWideTwoColumn(t *testing.T) {
	longVal := strings.Repeat("x", 60)
	in := "| Key | Value |\n|---|---|\n| name | " + longVal + " |"
	got := renderMarkdownTable(in)

	assert.Equal(t, "<b>name</b>: "+longVal, got)
	assert.NotContains(t, got, "<pre>")
}

func TestRenderMarkdownTableWideCards(t *testing.T) {
	longNote := strings.Repeat("y", 60)
	in := "| Name | Role | Note |\n|---|---|---|\n| Alice | Admin | " + longNote + " |"
	got := renderMarkdownTable(in)

	assert.Contains(t, got, "━━ Alice ━━")
	assert.Contains(t, got, "<b>Role</b>: Admin")
	assert.Contains(t, got, "<b>Note</b>: "+longNote)
}

func TestRenderMarkdownTableCyrillicAlignment(t *testing.T) {
	in := "| Имя | X |\n| Кот | 1 |"
	got := renderMarkdownTable(in)

	// Rune-based widths keep both data columns aligned despite multi-byte runes.
	require.True(t, strings.HasPrefix(got, "<pre>"))
	assert.Contains(t, got, "| Имя | X |")
	assert.Contains(t, got, "| Кот | 1 |")
}

func TestRenderMarkdownTableInlineCodeInWideCell(t *testing.T) {
	longVal := "`ENV`" + strings.Repeat("z", 60)
	in := "| Key | Value |\n|---|---|\n| k | " + longVal + " |"
	got := renderMarkdownTable(in)

	assert.Contains(t, got, "<code>ENV</code>")
}

func TestRenderMarkdownTableEscapesCells(t *testing.T) {
	longVal := strings.Repeat("z", 60)
	in := "| Key | Value |\n|---|---|\n| a<b | " + longVal + " |"
	got := renderMarkdownTable(in)

	assert.Contains(t, got, "a&lt;b")
	assert.NotContains(t, got, "a<b")
}

func TestDisableLinkPreview(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		params  map[string]any
		wantSet bool
	}{
		{"send message injects", "sendMessage", map[string]any{tgKeyText: "hi"}, true},
		{"edit message injects", "editMessageText", map[string]any{tgKeyText: "hi"}, true},
		{"callback untouched", "answerCallbackQuery", map[string]any{tgKeyText: "hi"}, false},
		{"typing untouched", "sendChatAction", map[string]any{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disableLinkPreview(tt.method, tt.params)

			opts, ok := tt.params[tgKeyLinkPreview]
			assert.Equal(t, tt.wantSet, ok)
			if tt.wantSet {
				assert.Equal(t, map[string]any{"is_disabled": true}, opts)
			}
		})
	}
}

func TestDisableLinkPreviewRespectsCaller(t *testing.T) {
	preset := map[string]any{"is_disabled": false}
	params := map[string]any{tgKeyText: "hi", tgKeyLinkPreview: preset}

	disableLinkPreview("sendMessage", params)

	assert.Equal(t, preset, params[tgKeyLinkPreview])
}
