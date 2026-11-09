package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

const resolutionsFileName = "resolved-models.json"

type (
	// Resolution is what a model's catalog last answered for it.
	Resolution struct {
		Origin     string    `json:"origin"`
		ResolvedAt time.Time `json:"resolved_at"`
		Spec       ModelSpec `json:"spec"`
	}

	// Resolutions caches each model/origin pair's last Resolution; losing it reinstates fail-closed boot.
	// The zero value has no file.
	Resolutions struct {
		dir     string
		entries map[string]map[string]Resolution
	}
)

// LoadResolutions treats an unreadable cache as absent so cache loss stays fail-closed.
func LoadResolutions(ctx context.Context) *Resolutions {
	return loadResolutionsFrom(ctx, defaultCacheDir())
}

// Recall keeps one provider origin's metadata from being used for another.
func (r *Resolutions) Recall(id string, provider config.ProviderEntry) (Resolution, bool) {
	entry, ok := r.entries[id][origin(provider)]

	return entry, ok
}

// Remember updates memory only; callers must validate the whole enrichment before Save.
func (r *Resolutions) Remember(id string, provider config.ProviderEntry, spec ModelSpec) {
	if r.entries == nil {
		r.entries = make(map[string]map[string]Resolution)
	}

	if r.entries[id] == nil {
		r.entries[id] = make(map[string]Resolution)
	}

	key := origin(provider)
	r.entries[id][key] = Resolution{Origin: key, ResolvedAt: time.Now().UTC(), Spec: spec}
}

// Save warns on failure because the current boot already has validated metadata.
func (r *Resolutions) Save(ctx context.Context) {
	if r.dir == "" {
		return
	}

	log := logger.Ctx(ctx).Named("catalog.resolutions")
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		log.Warn("catalog_resolutions_write_failed", zap.Error(err))

		return
	}

	body, err := json.MarshalIndent(r.entries, "", "  ")
	if err == nil {
		err = writeResolutions(r.dir, body)
	}

	if err != nil {
		log.Warn("catalog_resolutions_write_failed", zap.Error(err))
	}
}

func loadResolutionsFrom(ctx context.Context, dir string) *Resolutions {
	r := &Resolutions{dir: dir}
	if dir == "" {
		return r
	}

	body, err := os.ReadFile(filepath.Join(dir, resolutionsFileName))
	if errors.Is(err, os.ErrNotExist) {
		return r
	}

	if err == nil {
		err = json.Unmarshal(body, &r.entries)
	}

	if err != nil {
		logger.Ctx(ctx).Named("catalog.resolutions").Warn("catalog_resolutions_read_failed", zap.Error(err))

		r.entries = nil
	}

	return r
}

func writeResolutions(dir string, body []byte) error {
	f, err := os.CreateTemp(dir, ".resolved-models-*")
	if err != nil {
		return fmt.Errorf("create temp resolutions: %w", err)
	}

	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()

	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("write resolutions: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync resolutions: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close resolutions: %w", err)
	}

	if err := os.Rename(tmp, filepath.Join(dir, resolutionsFileName)); err != nil {
		return fmt.Errorf("rename resolutions: %w", err)
	}

	return nil
}

func origin(provider config.ProviderEntry) string {
	sum := sha256.Sum256([]byte(provider.Driver + "\x00" + provider.Catalog + "\x00" + provider.BaseURL))

	return hex.EncodeToString(sum[:])[:16]
}
