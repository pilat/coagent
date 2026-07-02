package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestHTMLToText(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{name: "script bodies are dropped", html: "<script>var a = 1;</script>keep", want: "keep"},
		{name: "style bodies are dropped", html: "<style>a{color:red}</style>keep", want: "keep"},
		{name: "comments are dropped", html: "<!-- hidden -->keep", want: "keep"},
		{name: "closing block tags become newlines", html: "<p>one</p><p>two</p>", want: "one\ntwo"},
		{name: "remaining tags are stripped", html: `<a href="/x">link</a>`, want: "link"},
		{
			name: "entities are decoded",
			html: "a &amp; b &lt;c&gt; &quot;d&quot; &apos;e&apos;",
			want: `a & b <c> "d" 'e'`,
		},
		{name: "non breaking spaces collapse", html: "a&nbsp;&nbsp;b", want: "a b"},
		{name: "runs of spaces and tabs collapse", html: "a \t  b", want: "a b"},
		{name: "long newline runs collapse to one blank line", html: "a\n\n\n\n\nb", want: "a\n\nb"},
		{name: "two newlines are preserved", html: "a\n\nb", want: "a\n\nb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, htmlToText(tt.html))
		})
	}
}

func TestWebFetchOutputTruncationBoundary(t *testing.T) {
	tests := []struct {
		name      string
		size      int
		truncated bool
	}{
		{name: "exactly at the cap is kept whole", size: maxWebFetchOutput, truncated: false},
		{name: "one byte over is cut", size: maxWebFetchOutput + 1, truncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Repeat("a", tt.size)
			result := fetch(t, "text/plain", http.StatusOK, body)

			assert.Equal(t, tt.truncated, result.Metadata[metaKeyTruncated])

			if tt.truncated {
				assert.Equal(t, strings.Repeat("a", maxWebFetchOutput)+"\n\n(Content truncated)", result.Output)
				return
			}

			assert.Equal(t, body, result.Output)
		})
	}
}

func TestWebFetchConvertsHTMLOnlyForHTMLContentType(t *testing.T) {
	const body = "<p>hello</p>"

	assert.Equal(t, "hello", fetch(t, "text/html; charset=utf-8", http.StatusOK, body).Output)
	assert.Equal(t, body, fetch(t, "text/plain", http.StatusOK, body).Output)
}

func TestWebFetchRejectsNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	raw, err := json.Marshal(webFetchParams{URL: server.URL})
	require.NoError(t, err)

	_, err = newWebFetchTool().Execute(context.Background(), raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 500")
}

func TestWebFetchRejectsBadInput(t *testing.T) {
	webTool := newWebFetchTool()

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "malformed json", raw: `{`, wantErr: "invalid parameters"},
		{name: "empty url", raw: `{"url":""}`, wantErr: "url is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := webTool.Execute(context.Background(), json.RawMessage(tt.raw))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func fetch(t *testing.T, contentType string, status int, body string) *tool.Result {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	raw, err := json.Marshal(webFetchParams{URL: server.URL})
	require.NoError(t, err)

	result, err := newWebFetchTool().Execute(context.Background(), raw)
	require.NoError(t, err)

	return result
}
