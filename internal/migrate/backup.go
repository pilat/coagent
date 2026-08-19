package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

func backupDB(ctx context.Context, db *sql.DB, dbPath string) error {
	bakPath := dbPath + ".bak"

	stageDir, err := os.MkdirTemp(filepath.Dir(bakPath), "."+filepath.Base(bakPath)+".stage-*")
	if err != nil {
		return fmt.Errorf("create private backup directory: %w", err)
	}

	defer func() { _ = os.RemoveAll(stageDir) }()

	tmpPath := filepath.Join(stageDir, "backup.db")

	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmpPath); err != nil {
		return fmt.Errorf("vacuum into temporary backup: %w", err)
	}

	if err := syncBackup(tmpPath); err != nil {
		return err
	}

	if err := publishBackup(tmpPath, bakPath); err != nil {
		return err
	}

	logger.Ctx(ctx).Named("migrate.backup").Info("created", zap.String("path", bakPath))

	return nil
}

func syncBackup(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set backup mode: %w", err)
	}

	if err := syncFile(path); err != nil {
		return fmt.Errorf("sync temporary backup: %w", err)
	}

	return nil
}

func publishBackup(tmpPath, bakPath string) error {
	oldPath, err := preserveBackup(bakPath)
	if err != nil {
		return err
	}

	if err := os.Rename(tmpPath, bakPath); err != nil {
		return errors.Join(fmt.Errorf("publish backup: %w", err), restoreBackup(oldPath, bakPath))
	}

	if err := syncDirectory(filepath.Dir(bakPath)); err != nil {
		removeErr := removeIncompleteBackup(bakPath)
		restoreErr := restoreBackup(oldPath, bakPath)

		return errors.Join(fmt.Errorf("sync backup directory: %w", err), removeErr, restoreErr)
	}

	if oldPath != "" {
		if err := os.Remove(oldPath); err != nil {
			return fmt.Errorf("remove previous backup: %w", err)
		}

		if err := syncDirectory(filepath.Dir(bakPath)); err != nil {
			return fmt.Errorf("sync previous backup removal: %w", err)
		}
	}

	return nil
}

func preserveBackup(bakPath string) (string, error) {
	info, err := os.Lstat(bakPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("inspect backup destination: %w", err)
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("backup destination is not a regular file: %s", bakPath)
	}

	old, err := os.CreateTemp(filepath.Dir(bakPath), "."+filepath.Base(bakPath)+".old-*")
	if err != nil {
		return "", fmt.Errorf("create previous backup placeholder: %w", err)
	}

	oldPath := old.Name()

	if err := preparePreservedBackup(old); err != nil {
		return "", err
	}

	if err := os.Link(bakPath, oldPath); err != nil {
		_ = os.Remove(oldPath)
		return "", fmt.Errorf("link previous backup: %w", err)
	}

	if err := syncDirectory(filepath.Dir(bakPath)); err != nil {
		_ = os.Remove(oldPath)
		return "", fmt.Errorf("sync previous backup link: %w", err)
	}

	return oldPath, nil
}

func preparePreservedBackup(old *os.File) error {
	path := old.Name()
	if err := old.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close previous backup placeholder: %w", err)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("prepare previous backup placeholder: %w", err)
	}

	return nil
}

func removeIncompleteBackup(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove incomplete backup: %w", err)
	}

	return nil
}

func restoreBackup(oldPath, bakPath string) error {
	if oldPath == "" {
		return nil
	}

	if err := os.Rename(oldPath, bakPath); err != nil {
		return fmt.Errorf("restore previous backup: %w", err)
	}

	if err := syncDirectory(filepath.Dir(bakPath)); err != nil {
		return fmt.Errorf("sync restored backup: %w", err)
	}

	return nil
}

func syncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open file for sync: %w", err)
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync file: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close synced file: %w", err)
	}

	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}

	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync directory: %w", err)
	}

	if err := directory.Close(); err != nil {
		return fmt.Errorf("close synced directory: %w", err)
	}

	return nil
}
