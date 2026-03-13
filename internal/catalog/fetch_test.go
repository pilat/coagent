package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

// The transport names no catalog, so its tests invent their own payload and
// validator — anything real would smuggle a driver's knowledge back in here.
const (
	goodBody = `{"models": ["a", "b"]}`
	junkBody = `<html>maintenance</html>`
)

func jsonValidator(body []byte) error {
	var parsed map[string]any

	return json.Unmarshal(body, &parsed)
}

func testSource(url string) Source {
	return Source{URL: url, CacheName: "probe.json", Validate: jsonValidator}
}

func TestFetchCachesAndMemoizesPerURL(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(goodBody))
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	f := New(WithCacheDir(cacheDir), WithHTTPClient(srv.Client()))

	first, err := f.Fetch(context.Background(), testSource(srv.URL))
	require.NoError(t, err)
	assert.JSONEq(t, goodBody, string(first))

	second, err := f.Fetch(context.Background(), testSource(srv.URL))
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, int32(1), hits.Load(), "one URL is fetched once per process")

	cached, err := os.ReadFile(filepath.Join(cacheDir, "probe.json"))
	require.NoError(t, err)
	assert.JSONEq(t, goodBody, string(cached))
}

func TestFetchMemoizesEachURLSeparately(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(goodBody))
	}))
	defer srv.Close()

	f := New(WithCacheDir(t.TempDir()), WithHTTPClient(srv.Client()))

	_, err := f.Fetch(context.Background(), testSource(srv.URL+"/one"))
	require.NoError(t, err)
	_, err = f.Fetch(context.Background(), testSource(srv.URL+"/two"))
	require.NoError(t, err)

	assert.Equal(t, int32(2), hits.Load(), "distinct sources must not share a memo slot")
}

func TestFetchFallsBackToCacheWhenOffline(t *testing.T) {
	cacheDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "probe.json"), []byte(goodBody), 0o600))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := New(WithCacheDir(cacheDir), WithHTTPClient(srv.Client()))

	body, err := f.Fetch(context.Background(), testSource(srv.URL))
	require.NoError(t, err)
	assert.JSONEq(t, goodBody, string(body))
}

func TestFetchFailsWhenOfflineWithoutCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := New(WithCacheDir(t.TempDir()), WithHTTPClient(srv.Client()))

	_, err := f.Fetch(context.Background(), testSource(srv.URL))
	assert.ErrorContains(t, err, "not cached")
}

func TestFetchKeepsGoodCacheWhenServerReturnsGarbage(t *testing.T) {
	cacheDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "probe.json"), []byte(goodBody), 0o600))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(junkBody))
	}))
	defer srv.Close()

	f := New(WithCacheDir(cacheDir), WithHTTPClient(srv.Client()))

	body, err := f.Fetch(context.Background(), testSource(srv.URL))
	require.NoError(t, err)
	assert.JSONEq(t, goodBody, string(body), "the validator rejected the body, so the cache answered")

	still, err := os.ReadFile(filepath.Join(cacheDir, "probe.json"))
	require.NoError(t, err)
	assert.JSONEq(t, goodBody, string(still), "a 200 carrying garbage must not clobber the cache")
}

func TestFetchFailsWhenBothLiveAndCachedBodiesAreUnusable(t *testing.T) {
	cacheDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "probe.json"), []byte(junkBody), 0o600))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(junkBody))
	}))
	defer srv.Close()

	f := New(WithCacheDir(cacheDir), WithHTTPClient(srv.Client()))

	_, err := f.Fetch(context.Background(), testSource(srv.URL))
	assert.ErrorContains(t, err, "cached copy is unusable")
}

// A source without a validator cannot vet anything, so whatever arrives is served.
func TestFetchWithoutValidatorAcceptsAnyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(junkBody))
	}))
	defer srv.Close()

	f := New(WithCacheDir(t.TempDir()), WithHTTPClient(srv.Client()))

	body, err := f.Fetch(context.Background(), Source{URL: srv.URL, CacheName: "probe.json"})
	require.NoError(t, err)
	assert.Equal(t, junkBody, string(body))
}

func TestFetchRejectsSourceWithoutURL(t *testing.T) {
	_, err := New(WithCacheDir(t.TempDir())).Fetch(context.Background(), Source{})
	assert.ErrorContains(t, err, "no url")
}

func TestFetchDerivesACacheNameWhenSourceOmitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(goodBody))
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	f := New(WithCacheDir(cacheDir), WithHTTPClient(srv.Client()))

	_, err := f.Fetch(context.Background(), Source{URL: srv.URL, Validate: jsonValidator})
	require.NoError(t, err)

	entries, err := os.ReadDir(cacheDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, CacheName(coagenthome.CatalogDirName, srv.URL), entries[0].Name())
}

func TestFetchMemoizesTheFailureToo(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := New(WithCacheDir(t.TempDir()), WithHTTPClient(srv.Client()))

	_, first := f.Fetch(context.Background(), testSource(srv.URL))
	_, second := f.Fetch(context.Background(), testSource(srv.URL))

	require.Error(t, first)
	assert.Equal(t, errors.Is(first, second), errors.Is(second, first))
	assert.Equal(t, int32(1), hits.Load(), "a dead catalog is not retried per driver")
}
