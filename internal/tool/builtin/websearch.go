package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/tool"
)

const (
	maxWebSearchOutput = 20000 // chars
	websearchToolID    = "websearch"
	// defaultSearchMaxResults mirrors config: both endpoints clamp to [1,10].
	defaultSearchMaxResults = 5
	maxSearchMaxResults     = 10

	websearchDescription = `Performs a web search and returns a ranked list of results (title, URL, snippet).

Use this for:
- Information beyond your knowledge cutoff
- Current documentation, recent changes, latest versions
- Finding URLs before fetching them with webfetch

Note: Results are search snippets only. Use webfetch to read a page's full content.`
)

var _ tool.Tool = (*webSearchTool)(nil)

type webSearchParams struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
}

type webSearchTool struct {
	provider searchProvider
	// maxResults is the config-clamped ceiling for one call.
	maxResults int
}

// newWebSearchTool requires a non-nil provider; callers only construct it when
// config selects a provider.
func newWebSearchTool(p searchProvider, maxResults int) *webSearchTool {
	return &webSearchTool{
		provider:   p,
		maxResults: clampSearchResults(maxResults),
	}
}

// clampSearchResults bounds a configured or requested result count.
func clampSearchResults(n int) int {
	if n <= 0 {
		return defaultSearchMaxResults
	}

	return min(n, maxSearchMaxResults)
}

func (t *webSearchTool) ID() string          { return websearchToolID }
func (t *webSearchTool) ParallelSafe() bool  { return true } // stateless HTTP
func (t *webSearchTool) Description() string { return websearchDescription }

func (t *webSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "The search query to execute"
			},
			"max_results": {
				"type": "integer",
				"description": "Maximum number of results to return (1-10, default 5)"
			}
		},
		"required": ["query"]
	}`)
}

func (t *webSearchTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p webSearchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if strings.TrimSpace(p.Query) == "" {
		return nil, errors.New("query is required")
	}

	// A call may narrow the configured count, never widen it.
	count := t.maxResults
	if p.MaxResults > 0 {
		count = min(p.MaxResults, t.maxResults)
	}

	results, err := t.provider.Search(ctx, p.Query, count)
	if err != nil {
		//nolint:wrapcheck // adapters already name their provider ("tavily: …")
		return nil, err
	}

	return renderSearchResults(p.Query, results)
}

// truncateRunes cuts to at most limit bytes without splitting a rune: slice to
// the byte cap, then back off any trailing continuation bytes.
func truncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	cut := s[:limit]
	for cut != "" && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}

	return cut
}

func renderSearchResults(query string, results []searchResult) (*tool.Result, error) {
	if len(results) == 0 {
		return &tool.Result{
			Title:  query,
			Output: "No results found.",
			Metadata: map[string]any{
				"query":          query,
				metaKeyTruncated: false,
				"result_count":   0,
			},
		}, nil
	}

	var sb strings.Builder

	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n   %s\n", i+1, r.Title, r.URL)

		if r.Snippet != "" {
			fmt.Fprintf(&sb, "   %s\n", strings.ReplaceAll(r.Snippet, "\n", " "))
		}
	}

	output := strings.TrimRight(sb.String(), "\n")
	truncated := false

	if len(output) > maxWebSearchOutput {
		output = strings.ToValidUTF8(truncateRunes(output, maxWebSearchOutput), string(utf8.RuneError)) +
			"\n\n(Results truncated)"
		truncated = true
	}

	return &tool.Result{
		Title:  query,
		Output: output,
		Metadata: map[string]any{
			"query":          query,
			metaKeyTruncated: truncated,
			"result_count":   len(results),
		},
	}, nil
}
