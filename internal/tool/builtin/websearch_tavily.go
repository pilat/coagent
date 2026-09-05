package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const tavilySearchEndpoint = "https://api.tavily.com/search"

// tavilySearchProvider speaks the Tavily Search REST contract
// (POST https://api.tavily.com/search, Bearer key). Content extraction stays
// off: SERP-style results only, page fetching is webfetch's job.
type tavilySearchProvider struct {
	client *http.Client
	apiKey string
	// endpoint defaults to the Tavily Search API; tests aim it at a stub.
	endpoint string
}

type tavilyRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

type tavilyDetail struct {
	Error string `json:"error"`
}

func (d *tavilyDetail) GetError() string {
	if d == nil {
		return ""
	}

	return d.Error
}

type tavilyResponse struct {
	Results []struct {
		Title   string  `json:"title"`
		URL     string  `json:"url"`
		Content string  `json:"content"`
		Score   float64 `json:"score"`
	} `json:"results"`
	// Tavily reports failures as {"detail":{"error":"..."}} with 4xx/5xx codes.
	Detail *tavilyDetail `json:"detail,omitempty"`
}

func (p *tavilySearchProvider) Search(ctx context.Context, query string, maxResults int) ([]searchResult, error) {
	endpoint := p.endpoint
	if endpoint == "" {
		endpoint = tavilySearchEndpoint
	}

	body, err := json.Marshal(tavilyRequest{Query: query, MaxResults: maxResults})
	if err != nil {
		return nil, fmt.Errorf("tavily: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tavily: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily: search request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWebFetchSize))
	if err != nil {
		return nil, fmt.Errorf("tavily: read response: %w", err)
	}

	var parsed tavilyResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("tavily: malformed response (HTTP %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := parsed.Detail.GetError()
		if msg == "" {
			msg = excerptResponse(raw)
		}

		return nil, fmt.Errorf("tavily: HTTP %d: %s", resp.StatusCode, msg)
	}

	out := make([]searchResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}

	return out, nil
}
