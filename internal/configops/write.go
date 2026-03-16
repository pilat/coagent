package configops

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// backupStamp is the timestamp format of config.yaml.bak.<ts>. Sortable as text,
// which is what makes pruning a plain lexical sort.
const backupStamp = "20060102-150405"

// backupRetention bounds how many timestamped backups survive. Old enough to
// undo a bad afternoon, bounded enough not to become a directory of noise.
const backupRetention = 20

// backupSuffix marks the machine-written backups this package prunes. Anything
// else in the directory is the human's and is never touched.
const backupSuffix = ".bak."

// backupConfig copies the current file aside and returns the backup path. A
// missing file is not an error — the first write on a fresh install has nothing
// to preserve, and reports an empty path.
func backupConfig(path, stamp string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("read config for backup: %w", err)
	}

	bak := path + backupSuffix + stamp
	if err := os.WriteFile(bak, data, 0o600); err != nil {
		return "", fmt.Errorf("write config backup: %w", err)
	}

	return bak, nil
}

// pruneBackups keeps the newest backupRetention backups. Failure to prune is not
// failure to save — the config is already written by the time this runs.
func pruneBackups(path string) {
	dir := filepath.Dir(path)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	prefix := filepath.Base(path) + backupSuffix

	var backups []string

	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			backups = append(backups, e.Name())
		}
	}

	if len(backups) <= backupRetention {
		return
	}

	// The stamp format sorts lexically, so newest-last needs no time parsing.
	slices.Sort(backups)

	for _, name := range backups[:len(backups)-backupRetention] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func writeConfigFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	return writeFileAtomic(path, data)
}

// writeFileAtomic writes via a temp file in the same directory plus a rename, so
// a crash mid-write leaves the old file intact rather than a truncated one.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	tmp := f.Name()

	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}

	return syncDir(filepath.Dir(path))
}

// syncDir commits the rename itself, not just the bytes: without it the
// marker-before-config write order cannot survive a power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for sync: %w", err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync dir: %w", err)
	}

	return nil
}
