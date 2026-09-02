package loader

import (
	"context"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/logger"
)

// recoverMarketplace swaps a clone for a fresh one when the clone fails a git
// fsck probe; the stale clone is parked by rename before anything is destroyed,
// so every failure leaves a usable copy and degradation stays unchanged.
func recoverMarketplace(ctx context.Context, gitClient git.Client, repoURL, cacheDir string) {
	healthErr := gitClient.HealthCheck(ctx, cacheDir)
	if healthErr == nil {
		// Pull failed but the clone itself is sound (network/auth): keep serving it.
		return
	}

	logger.Named("loader").Warn("marketplace_health_check_failed",
		zap.String("url", repoURL), zap.Error(healthErr))

	tmp, err := os.MkdirTemp(filepath.Dir(cacheDir), filepath.Base(cacheDir)+".recovery-*")
	if err != nil {
		recoveryFailed(repoURL, "create temp dir", err)

		return
	}

	// Clone refuses an existing destination; MkdirTemp only reserved the name.
	_ = os.Remove(tmp)

	defer func() { _ = os.RemoveAll(tmp) }()

	if err := gitClient.Clone(ctx, repoURL, tmp); err != nil {
		recoveryFailed(repoURL, "clone", err)

		return
	}

	// tmp is MkdirTemp-unique, so parking the stale clone under tmp+".old" cannot collide.
	old := tmp + ".old"

	if err := os.Rename(cacheDir, old); err != nil {
		recoveryFailed(repoURL, "park stale clone", err)

		return
	}

	if err := os.Rename(tmp, cacheDir); err != nil {
		recoveryFailed(repoURL, "swap in fresh clone", err)

		if renameErr := os.Rename(old, cacheDir); renameErr != nil {
			// Rollback failed: stale stays parked, the next resolve clones into the empty slot.
			recoveryFailed(repoURL, "restore stale clone", renameErr)
		}

		return
	}

	// A leftover parked dir is a complete usable clone; harmless to leave behind.
	_ = os.RemoveAll(old)

	logger.Named("loader").Info("marketplace_recovered", zap.String("url", repoURL))
}

func recoveryFailed(repoURL, step string, err error) {
	logger.Named("loader").Warn("marketplace_recovery_failed",
		zap.String("url", repoURL), zap.String("step", step), zap.Error(err))
}
