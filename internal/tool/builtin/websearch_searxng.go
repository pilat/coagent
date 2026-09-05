package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// searxngSearchProvider speaks a SearXNG instance's JSON API
// (GET {base_url}/search?q=...&format=json). The instance must enable JSON
// output — `formats: [html, json]` in settings.yml — and refuses the format
// with HTTP 403 otherwise.
type searxngSearchProvider struct {
	client  *http.Client
	baseURL string
}

type searxngResponse struct {
	Results []struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Content string `json:"content"`
	} `json:"results"`
}

func (p *searxngSearchProvider) Search(ctx context.Context, query string, maxResults int) ([]searchResult, error) {
	endpoint := strings.TrimRight(p.baseURL, "/") + "/search"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("searxng: create request: %w", err)
	}

	q := req.URL.Query()
	q.Set("q", query)
	q.Set("format", "json")
	req.URL.RawQuery = q.Encode()

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng: search request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWebFetchSize))
	if err != nil {
		return nil, fmt.Errorf("searxng: read response: %w", err)
	}

	// flask.abort(403) is the instance's format gate: JSON not enabled in
	// search.formats. Other statuses surface verbatim — never parse garbage.
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf(
				"searxng: HTTP 403 — the instance does not allow JSON output; "+
					"add \"json\" to \"formats\" under the \"search:\" section of its settings.yml "+
					"(instance: %s)", p.baseURL,
			)
		}

		return nil, fmt.Errorf("searxng: HTTP %d: %s", resp.StatusCode, excerptResponse(raw))
	}

	var parsed searxngResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("searxng: malformed response (HTTP %d): %w", resp.StatusCode, err)
	}

	// The JSON API has no result-count request parameter (pagination is pageno),
	// so the requested count is honored client-side.
	results := parsed.Results
	if len(results) > maxResults {
		results = results[:maxResults]
	}

	out := make([]searchResult, 0, len(results))
	for _, r := range results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}

	return out, nil
}

// excerptResponse bounds a raw error body for an error message.
func excerptResponse(body []byte) string {
	const limit = 200

	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		return text[:limit]
	}

	return text
}
