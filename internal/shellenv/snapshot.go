package shellenv

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Snapshot ensures a fresh snapshot for workDir and returns its path, or "" when
// unavailable. It never returns an error — every failure degrades to "".
func (p *provider) Snapshot(ctx context.Context, workDir string) string {
	if p.shell == "" || workDir == "" {
		return ""
	}

	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return ""
	}

	dir, err := p.ensureCacheDir()
	if err != nil {
		p.log().Warn("cache_dir_failed", zap.Error(err))
		return ""
	}

	path := filepath.Join(dir, p.cacheKey(workDir))
	fp := p.fingerprint(workDir)

	if p.valid(workDir, path, fp) {
		return path
	}

	unlock := p.lockKey(path)
	defer unlock()

	// Re-check under the lock: a racing first-spawn may have captured already.
	if p.valid(workDir, path, fp) {
		return path
	}

	p.captureN.Add(1)

	content, err := p.captureFn(ctx, workDir)
	if err != nil {
		p.log().Warn("capture_failed", zap.String("workDir", workDir), zap.Error(err))
		return ""
	}

	if err := writeSnapshot(dir, path, content); err != nil {
		p.log().Warn("write_failed", zap.Error(err))
		return ""
	}

	p.recordFP(workDir, fp)

	return path
}

// Fingerprint hashes the on-disk state that determines workDir's activated env.
func (p *provider) Fingerprint(workDir string) string {
	if p.shell == "" || workDir == "" {
		return ""
	}

	return p.fingerprint(workDir)
}

// Invalidate drops workDir's recorded fingerprint so the next Snapshot recaptures.
func (p *provider) Invalidate(workDir string) {
	p.fpMu.Lock()
	delete(p.fps, workDir)
	p.fpMu.Unlock()
}

// valid reports whether the cached snapshot for workDir is still usable: the file
// is present and within the backstop TTL, and its fingerprint still matches.
func (p *provider) valid(workDir, path, fp string) bool {
	if !p.fresh(path) {
		return false
	}

	p.fpMu.Lock()
	prev, ok := p.fps[workDir]
	p.fpMu.Unlock()

	return ok && prev == fp
}

func (p *provider) recordFP(workDir, fp string) {
	p.fpMu.Lock()
	if p.fps == nil {
		p.fps = make(map[string]string)
	}

	p.fps[workDir] = fp
	p.fpMu.Unlock()
}

func (p *provider) cacheKey(workDir string) string {
	h := sha512.New()
	h.Write([]byte(workDir))
	h.Write(p.salt)

	return hex.EncodeToString(h.Sum(nil))
}

func (p *provider) fresh(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return time.Since(info.ModTime()) < p.ttl
}

// ensureCacheDir lazily creates a 0700 per-instance dir under UserCacheDir,
// which is bwrap-visible via `--ro-bind / /` so the replay `source` reaches it.
func (p *provider) ensureCacheDir() (string, error) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	if p.cacheDir != "" {
		return p.cacheDir, nil
	}

	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}

	dir := filepath.Join(base, "coagent", "shellenv", p.instanceID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	// MkdirAll honors umask; force 0700 so at-rest operator creds in snapshots
	// stay owner-only.
	//nolint:gosec // G302 targets files; 0700 is correct for an owner-only dir
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("chmod cache dir: %w", err)
	}

	p.cacheDir = dir

	return dir, nil
}

func (p *provider) lockKey(key string) func() {
	actual, _ := p.keyLocks.LoadOrStore(key, &sync.Mutex{})
	mu, _ := actual.(*sync.Mutex)
	mu.Lock()

	return mu.Unlock
}

// writeSnapshot atomically installs content at path with 0600 perms.
func writeSnapshot(dir, path string, content []byte) error {
	f, err := os.CreateTemp(dir, "snap-*")
	if err != nil {
		return fmt.Errorf("create temp snapshot: %w", err)
	}

	tmp := f.Name()

	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)

		return fmt.Errorf("write snapshot: %w", err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("close snapshot: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("rename snapshot: %w", err)
	}

	return nil
}
