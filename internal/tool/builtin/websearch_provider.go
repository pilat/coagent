package builtin

import "context"

// searchResult is one SERP-style hit, normalized across providers.
type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// searchProvider is the thin adapter boundary a concrete search backend
// implements. Adapters own their transport and must name their provider in
// every error ("tavily: ...").
type searchProvider interface {
	Search(ctx context.Context, query string, maxResults int) ([]searchResult, error)
}
