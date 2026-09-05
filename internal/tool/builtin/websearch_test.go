package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

// searchServer spins an httptest server exposing one POST endpoint that
// captures the request and replies with status/body.
func searchServer(t *testing.T, status int, body string, seen func(r *http.Request, raw []byte)) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		if seen != nil {
			seen(r, raw)
		}

		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))

	t.Cleanup(srv.Close)

	return srv
}

// The Tavily adapter drives a recorded fixture shaped from the live API docs
// (POST https://api.tavily.com/search, Bearer key, results[].title/url/content).
func TestTavilySearchProvider_ParsesResults(t *testing.T) {
	t.Parallel()

	var gotPath, gotAuth, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		gotBody = string(raw)

		_, _ = w.Write([]byte(`{
			"query": "who is Leo Messi?",
			"results": [
				{
					"title": "Lionel Messi Facts | Britannica",
					"url": "https://www.britannica.com/facts/Lionel-Messi",
					"content": "Lionel Messi, an Argentine footballer, is widely regarded as one of the greatest.",
					"score": 0.81025416,
					"raw_content": null
				},
				{
					"title": "Lionel Messi | Biography",
					"url": "https://www.britannica.com/biography/Lionel-Messi",
					"content": "Born in 1987...",
					"score": 0.71
				}
			],
			"response_time": 1.67
		}`))
	}))

	t.Cleanup(srv.Close)

	p := newTestTavilyProvider(t, srv.URL)

	results, err := p.Search(t.Context(), "who is Leo Messi?", 5)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "Lionel Messi Facts | Britannica", results[0].Title)
	assert.Equal(t, "https://www.britannica.com/facts/Lionel-Messi", results[0].URL)
	assert.Contains(t, results[0].Snippet, "widely regarded as one of the greatest")

	// Path and auth ride every request per the API contract.
	assert.Equal(t, "/search", gotPath)
	assert.Equal(t, "Bearer tvly-test-key", gotAuth)

	var body struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	require.NoError(t, json.Unmarshal([]byte(gotBody), &body))
	assert.Equal(t, "who is Leo Messi?", body.Query)
	assert.Equal(t, 5, body.MaxResults)
}

// newTestTavilyProvider aims the adapter at a stub server. The /search path
// suffix mirrors the recorded API contract (POST https://api.tavily.com/search).
//
//nolint:gosec // fixture credential, never leaves the test process
func newTestTavilyProvider(t *testing.T, baseURL string) *tavilySearchProvider {
	t.Helper()

	return &tavilySearchProvider{
		client:   http.DefaultClient,
		apiKey:   "tvly-test-key",
		endpoint: baseURL + "/search",
	}
}

func TestTavilySearchProvider_APIErrorSurfacesDetail(t *testing.T) {
	t.Parallel()

	srv := searchServer(
		t,
		http.StatusUnauthorized,
		`{"detail":{"error":"Unauthorized: missing or invalid API key."}}`,
		nil,
	)
	p := newTestTavilyProvider(t, srv.URL)

	_, err := p.Search(t.Context(), "q", 5)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tavily:")
	assert.Contains(t, err.Error(), "Unauthorized: missing or invalid API key.")
}

func TestTavilySearchProvider_MalformedBodyIsProviderFailure(t *testing.T) {
	t.Parallel()

	srv := searchServer(t, http.StatusOK, `<html>not json</html>`, nil)
	p := newTestTavilyProvider(t, srv.URL)

	_, err := p.Search(t.Context(), "q", 5)

	require.Error(t, err)
	assert.ErrorContains(t, err, "tavily:")
}

// SearXNG normal results (recorded shape: results[].url/title/content).
func TestSearxngSearchProvider_ParsesResults(t *testing.T) {
	t.Parallel()

	var gotURL string

	// The instance returns more results than requested: SearXNG's JSON API has
	// no result-count parameter, so the adapter must trim client-side.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = w.Write([]byte(`{
			"query": "go release notes",
			"results": [
				{"url": "https://go.dev/doc/devel/release", "title": "Release History", "content": "Go 1.22 released."},
				{"url": "https://go.dev/blog/go1.22", "title": "Go 1.22 is released", "content": "loop var semantics."},
				{"url": "https://go.dev/ref/spec", "title": "The Go Programming Language Specification", "content": "spec."},
				{"url": "https://go.dev/blog/go1.23", "title": "Go 1.23 is released", "content": "iterators."}
			]
		}`))
	}))

	t.Cleanup(srv.Close)

	p := &searxngSearchProvider{client: srv.Client(), baseURL: srv.URL}

	results, err := p.Search(t.Context(), "go release notes", 2)
	require.NoError(t, err)
	require.Len(t, results, 2, "the requested count must trim the instance's page")
	assert.Equal(t, "Release History", results[0].Title)
	assert.Equal(t, "https://go.dev/doc/devel/release", results[0].URL)
	assert.Equal(t, "https://go.dev/blog/go1.22", results[1].URL, "trimming keeps instance order")

	// Query and JSON format ride the request; the user's query is URL-encoded.
	assert.Contains(t, gotURL, "q=go+release+notes")
	assert.Contains(t, gotURL, "format=json")
	assert.True(t, strings.HasPrefix(gotURL, "/search?"), "hits the instance's /search endpoint")
}

// A SearXNG instance with JSON disabled answers flask.abort(403); the error
// must tell the operator how to fix the instance, not show parse garbage.
func TestSearxngSearchProvider_JSONDisabledSurfacesActionableError(t *testing.T) {
	t.Parallel()

	srv := searchServer(t, http.StatusForbidden, "403 Forbidden\n", nil)
	p := &searxngSearchProvider{client: srv.Client(), baseURL: srv.URL}

	_, err := p.Search(t.Context(), "q", 5)

	require.Error(t, err)
	require.ErrorContains(t, err, "searxng: HTTP 403")
	assert.ErrorContains(t, err, `add "json" to "formats"`)
}

func TestSearxngSearchProvider_TrailingSlashBaseURL(t *testing.T) {
	t.Parallel()

	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"results": []}`))
	}))

	t.Cleanup(srv.Close)

	p := &searxngSearchProvider{client: srv.Client(), baseURL: srv.URL + "/"}

	_, err := p.Search(t.Context(), "q", 5)
	require.NoError(t, err)
	assert.Equal(t, "/search", gotPath, "trailing slash on base_url must not double the slash")
}

// stubSearchProvider records the last Search call for tool-level tests.
type stubSearchProvider struct {
	results []searchResult
	err     error
	gotMax  int
	query   string
}

func (s *stubSearchProvider) Search(_ context.Context, query string, maxResults int) ([]searchResult, error) {
	s.gotMax = maxResults
	s.query = query

	return s.results, s.err
}

func TestWebSearchTool_HappyPathAndMetadata(t *testing.T) {
	t.Parallel()

	provider := &stubSearchProvider{results: []searchResult{
		{Title: "First", URL: "https://a.example.com", Snippet: "first snippet"},
		{URL: "https://b.example.com", Snippet: "second snippet\nwith newline"},
	}}
	tl := newWebSearchTool(provider, 5)

	require.True(t, tl.ParallelSafe())
	assert.Equal(t, "websearch", tl.ID())

	result, err := tl.Execute(t.Context(), json.RawMessage(`{"query":"test"}`))
	require.NoError(t, err)
	assert.Equal(t, "test", result.Title)
	assert.Equal(t, false, result.Metadata[metaKeyTruncated])
	assert.Equal(t, 2, result.Metadata["result_count"])
	assert.Contains(t, result.Output, "1. First")
	assert.Contains(t, result.Output, "https://a.example.com")
	assert.Contains(t, result.Output, "second snippet with newline")
	assert.Equal(t, 5, provider.gotMax)
}

func TestWebSearchTool_PerCallMaxResultsNeverWidens(t *testing.T) {
	t.Parallel()

	provider := &stubSearchProvider{results: []searchResult{}}
	tl := newWebSearchTool(provider, 3)

	_, err := tl.Execute(t.Context(), json.RawMessage(`{"query":"q","max_results":50}`))
	require.NoError(t, err)
	assert.Equal(t, 3, provider.gotMax, "a call narrows the ceiling, never widens it")
}

func TestWebSearchTool_EmptyAndInvalid(t *testing.T) {
	t.Parallel()

	tl := newWebSearchTool(&stubSearchProvider{}, 5)

	_, err := tl.Execute(t.Context(), json.RawMessage(`{"query":"  "}`))
	require.Error(t, err)
	require.ErrorContains(t, err, "query is required")

	_, err = tl.Execute(t.Context(), json.RawMessage(`not json`))
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid parameters")
}

func TestWebSearchTool_ProviderErrorPassthrough(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("tavily: HTTP 500: boom")
	tl := newWebSearchTool(&stubSearchProvider{err: wantErr}, 5)

	_, err := tl.Execute(t.Context(), json.RawMessage(`{"query":"q"}`))
	require.ErrorIs(t, err, wantErr)
}

func TestWebSearchTool_TruncatesLargeOutput(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 12000)
	provider := &stubSearchProvider{results: []searchResult{
		{Title: "a", URL: "https://a", Snippet: long},
		{Title: "b", URL: "https://b", Snippet: long},
	}}
	tl := newWebSearchTool(provider, 5)

	result, err := tl.Execute(t.Context(), json.RawMessage(`{"query":"q"}`))
	require.NoError(t, err)
	assert.Equal(t, true, result.Metadata[metaKeyTruncated])
	assert.Less(t, len(result.Output), len(long)*2)
}

var _ tool.Tool = (*webSearchTool)(nil)
