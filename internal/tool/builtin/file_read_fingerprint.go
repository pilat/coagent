package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pilat/coagent/internal/safefile"
)

//nolint:wsl_v5 // Stat and full-byte hashing must describe one observed version.
func recordFingerprint(access safefile.Access, workDir, name string) (string, ReadRecord, error) {
	path, err := resolveAccessTarget(access, workDir, name)
	if err != nil {
		return "", ReadRecord{}, err
	}
	key, err := ledgerPath(access, workDir, name)
	if err != nil {
		return "", ReadRecord{}, err
	}

	var info os.FileInfo
	if access != nil && access.Scope() == safefile.ProjectConfined {
		info, _, err = access.Stat(path)
	} else {
		info, err = os.Stat(path)
	}
	if err != nil {
		return "", ReadRecord{}, fmt.Errorf("stat file for read ledger: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", ReadRecord{}, fmt.Errorf("not a regular file: %s", path)
	}

	content, err := readAccessFile(access, path)
	if err != nil {
		return "", ReadRecord{}, fmt.Errorf("hash file for read ledger: %w", err)
	}
	sum := sha256.Sum256(content)

	return key, ReadRecord{
		MtimeUnixNano: info.ModTime().UnixNano(),
		Size:          info.Size(),
		Hash:          hex.EncodeToString(sum[:]),
	}, nil
}

func canonicalExistingPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}

	return filepath.Clean(path)
}

//nolint:wsl_v5 // Access resolution and host fallback are one canonicalization rule.
func ledgerPath(access safefile.Access, workDir, name string) (string, error) {
	if access != nil {
		resolved, err := access.Resolve(name)
		if err != nil {
			return "", fmt.Errorf("resolve ledger path: %w", err)
		}
		return resolved.Canonical, nil
	}

	return canonicalExistingPath(resolvePath(workDir, name)), nil
}
