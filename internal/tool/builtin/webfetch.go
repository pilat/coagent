package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/tool"
)

const (
	maxWebFetchSize     = 1024 * 1024 // 1MB
	webFetchTimeout     = 30 * time.Second
	maxWebFetchOutput   = 50000 // chars
	webfetchDescription = `Fetches content from a URL and returns the text.

Usage:
- Only http and https URLs are supported
- A URL without a scheme is fetched over HTTPS; write http:// explicitly for plain HTTP
- Link-local and cloud metadata addresses are refused and cannot be reached with this tool
- HTML is converted to plain text
- Content is limited to ~50K characters

Use this for:
- Reading documentation
- Fetching API responses
- Getting content from web pages

Note: Some sites may block automated requests.`
)

var _ tool.Tool = (*webFetchTool)(nil)

type webFetchParams struct {
	URL string `json:"url"`
}

type webFetchTool struct {
	client *http.Client
}

func newWebFetchTool() *webFetchTool {
	return &webFetchTool{
		client: &http.Client{
			Timeout:   webFetchTimeout,
			Transport: newRestrictedTransport(),
		},
	}
}

func (t *webFetchTool) ID() string          { return "webfetch" }
func (t *webFetchTool) ParallelSafe() bool  { return true }
func (t *webFetchTool) Description() string { return webfetchDescription }

func (t *webFetchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "The URL to fetch"
			}
		},
		"required": ["url"]
	}`)
}

func (t *webFetchTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p webFetchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.URL == "" {
		return nil, errors.New("url is required")
	}

	fetchURL, err := normalizeFetchURL(p.URL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", "coagent/1.0 (AI coding assistant)")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch URL: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWebFetchSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	content := string(body)
	contentType := resp.Header.Get("Content-Type")

	if strings.Contains(contentType, "text/html") {
		content = htmlToText(content)
	}

	truncated := false

	if len(content) > maxWebFetchOutput {
		content = content[:maxWebFetchOutput]
		truncated = true
	}

	output := content
	if truncated {
		output += "\n\n(Content truncated)"
	}

	return &tool.Result{
		Title:  fetchURL,
		Output: output,
		Metadata: map[string]any{
			"url":            fetchURL,
			"contentType":    contentType,
			metaKeyTruncated: truncated,
		},
	}, nil
}

// normalizeFetchURL resolves a caller-supplied URL to an absolute http/https one. A bare host gets
// https://, but an explicit http:// URL stays plain HTTP — services under development live on it.
func normalizeFetchURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)

	// Decides the branch before parsing: url.Parse("localhost:3000") yields scheme "localhost".
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported url scheme %q: only http and https are supported", u.Scheme)
	}

	if u.Host == "" {
		return "", fmt.Errorf("url has no host: %q", raw)
	}

	return u.String(), nil
}

// htmlToText converts HTML to plain text.
func htmlToText(html string) string {
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")

	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")

	commentRe := regexp.MustCompile(`(?s)<!--.*?-->`)
	html = commentRe.ReplaceAllString(html, "")

	blockRe := regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|br|hr)[^>]*>`)
	html = blockRe.ReplaceAllString(html, "\n")

	tagRe := regexp.MustCompile(`<[^>]+>`)
	html = tagRe.ReplaceAllString(html, "")

	html = strings.ReplaceAll(html, "&amp;", "&")
	html = strings.ReplaceAll(html, "&lt;", "<")
	html = strings.ReplaceAll(html, "&gt;", ">")
	html = strings.ReplaceAll(html, "&quot;", "\"")
	html = strings.ReplaceAll(html, "&apos;", "'")
	html = strings.ReplaceAll(html, "&nbsp;", " ")

	spaceRe := regexp.MustCompile(`[ \t]+`)
	html = spaceRe.ReplaceAllString(html, " ")

	newlineRe := regexp.MustCompile(`\n{3,}`)
	html = newlineRe.ReplaceAllString(html, "\n\n")

	return strings.TrimSpace(html)
}
