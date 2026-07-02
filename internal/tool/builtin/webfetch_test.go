package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const blockedTestAddr = "169.254.169.254"

func TestWebFetchRedirectToBlockedAddressIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+blockedTestAddr+"/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	_, err := newWebFetchTool().Execute(context.Background(), webFetchParamsJSON(t, srv.URL))

	require.Error(t, err)
	assert.Contains(t, err.Error(), blockedTestAddr)
	assert.Contains(t, err.Error(), "is blocked")
}

func TestWebFetchDirectBlockedURLIsRefused(t *testing.T) {
	_, err := newWebFetchTool().Execute(
		context.Background(),
		webFetchParamsJSON(t, "http://"+blockedTestAddr+"/"),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), blockedTestAddr)
	assert.Contains(t, err.Error(), "is blocked")
}

func TestWebFetchLoopbackOverPlainHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "hello from loopback")
	}))
	defer srv.Close()

	result, err := newWebFetchTool().Execute(context.Background(), webFetchParamsJSON(t, srv.URL))

	require.NoError(t, err)
	assert.Contains(t, result.Output, "hello from loopback")
	assert.Equal(t, srv.URL, result.Metadata["url"])
}

func TestWebFetchNormalizeURLUnsupportedSchemes(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantScheme string
	}{
		{"file", "file:///etc/passwd", "file"},
		{"ftp", "ftp://example.com/x", "ftp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeFetchURL(tt.raw)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantScheme)
		})
	}
}

func TestWebFetchNormalizeURLBareHostGetsHTTPS(t *testing.T) {
	normalized, err := normalizeFetchURL("example.com/docs")

	require.NoError(t, err)
	assert.Equal(t, "https://example.com/docs", normalized)
}

func TestWebFetchNormalizeURLKeepsExplicitHTTP(t *testing.T) {
	normalized, err := normalizeFetchURL("  http://localhost:3000/api  ")

	require.NoError(t, err)
	assert.Equal(t, "http://localhost:3000/api", normalized)
}

func TestWebFetchNormalizeURLEmptyHost(t *testing.T) {
	_, err := normalizeFetchURL("https:///path")

	require.Error(t, err)
}

func TestWebFetchSchemaOnlyPromisesImplementedInput(t *testing.T) {
	schema := string(newWebFetchTool().Parameters())

	assert.Contains(t, schema, `"url"`)
	assert.NotContains(t, schema, `"prompt"`, "webfetch does not perform model-backed extraction")
}

func webFetchParamsJSON(t *testing.T, rawURL string) json.RawMessage {
	t.Helper()

	params, err := json.Marshal(webFetchParams{URL: rawURL})
	require.NoError(t, err)

	return params
}
