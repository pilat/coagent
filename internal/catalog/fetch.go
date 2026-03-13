package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/logger"
)

const (
	fetchTimeout    = 5 * time.Second
	maxCatalogBytes = 64 << 20
)

var _ Fetcher = (*fetcher)(nil)

type (
	// Source describes one catalog endpoint. The driver owns every field: this
	// package names no endpoint and knows no payload format.
	Source struct {
		// URL is the endpoint to GET.
		URL string
		// CacheName is the disk cache filename. Empty derives one from the URL.
		CacheName string
		// Validate parses the body; a body it rejects is never cached and never
		// returned, so a 200 carrying garbage cannot clobber a good snapshot.
		Validate func([]byte) error
	}

	// Fetcher retrieves catalog bodies over HTTP with a disk-cache fallback. Each
	// URL is fetched at most once per process, so drivers sharing one catalog pay once.
	Fetcher interface {
		Fetch(ctx context.Context, src Source) ([]byte, error)
	}

	// Option configures a Fetcher. The HTTP and cache seams exist for tests.
	Option func(*fetcher)

	fetcher struct {
		http     *http.Client
		cacheDir string

		mu   sync.Mutex
		memo map[string]*memoEntry
	}

	memoEntry struct {
		once sync.Once
		body []byte
		err  error
	}
)

// New builds a Fetcher caching under ~/.coagent/cache/catalog. An unresolvable
// home directory disables the cache rather than failing — the network path still works.
func New(opts ...Option) Fetcher {
	f := &fetcher{
		http:     &http.Client{Timeout: fetchTimeout},
		cacheDir: defaultCacheDir(),
		memo:     make(map[string]*memoEntry),
	}

	for _, opt := range opts {
		opt(f)
	}

	return f
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(f *fetcher) { f.http = c }
}

// WithCacheDir overrides the disk cache location. Empty disables the cache.
func WithCacheDir(dir string) Option {
	return func(f *fetcher) { f.cacheDir = dir }
}

// CacheName derives a stable per-URL cache filename, for drivers whose endpoint
// is configurable and therefore cannot use a fixed name.
func CacheName(prefix, url string) string {
	sum := sha256.Sum256([]byte(url))

	return prefix + "-" + hex.EncodeToString(sum[:])[:8] + ".json"
}

func (f *fetcher) Fetch(ctx context.Context, src Source) ([]byte, error) {
	if src.URL == "" {
		return nil, errors.New("catalog source has no url")
	}

	entry := f.entry(src.URL)
	entry.once.Do(func() { entry.body, entry.err = f.load(ctx, src) })

	return entry.body, entry.err
}

func (f *fetcher) entry(url string) *memoEntry {
	f.mu.Lock()
	defer f.mu.Unlock()

	e, ok := f.memo[url]
	if !ok {
		e = &memoEntry{}
		f.memo[url] = e
	}

	return e
}

// load fetches the source, falling back to the last good disk snapshot. Validation
// gates both directions: an unusable body never reaches the caller or the cache.
func (f *fetcher) load(ctx context.Context, src Source) ([]byte, error) {
	log := logger.Ctx(ctx).Named("catalog.fetch")
	cacheName := src.cacheName()

	body, err := f.get(ctx, src.URL)
	if err == nil {
		err = src.validate(body)
		if err == nil {
			f.writeCache(cacheName, body)

			return body, nil
		}
	}

	cached, cacheErr := f.readCache(cacheName)
	if cacheErr != nil {
		return nil, fmt.Errorf("catalog %s unreachable and not cached: %w", src.URL, err)
	}

	if validateErr := src.validate(cached); validateErr != nil {
		return nil, fmt.Errorf(
			"catalog %s unreachable (%w) and cached copy is unusable: %w", src.URL, err, validateErr)
	}

	log.Warn("catalog_fetch_failed_using_cache", zap.String("url", src.URL), zap.Error(err))

	return cached, nil
}

func (s Source) cacheName() string {
	if s.CacheName != "" {
		return s.CacheName
	}

	return CacheName(coagenthome.CatalogDirName, s.URL)
}

func (s Source) validate(body []byte) error {
	if s.Validate == nil {
		return nil
	}

	return s.Validate(body)
}

func (f *fetcher) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build catalog request: %w", err)
	}

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch catalog: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch catalog: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes))
	if err != nil {
		return nil, fmt.Errorf("read catalog body: %w", err)
	}

	return body, nil
}

func (f *fetcher) readCache(name string) ([]byte, error) {
	if f.cacheDir == "" {
		return nil, os.ErrNotExist
	}

	body, err := os.ReadFile(filepath.Join(f.cacheDir, name))
	if err != nil {
		return nil, fmt.Errorf("read catalog cache: %w", err)
	}

	return body, nil
}

func (f *fetcher) writeCache(name string, body []byte) {
	if f.cacheDir == "" {
		return
	}

	log := logger.Named("catalog.cache")

	if err := os.MkdirAll(f.cacheDir, 0o755); err != nil {
		log.Warn("catalog_cache_mkdir_failed", zap.Error(err))

		return
	}

	if err := os.WriteFile(filepath.Join(f.cacheDir, name), body, 0o600); err != nil {
		log.Warn("catalog_cache_write_failed", zap.Error(err))
	}
}

func defaultCacheDir() string {
	p, err := coagenthome.Join(coagenthome.CacheDirName, coagenthome.CatalogDirName)
	if err != nil {
		return ""
	}

	return p
}
