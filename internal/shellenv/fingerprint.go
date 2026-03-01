package shellenv

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/pilat/coagent/internal/coagenthome"
)

const maxWalkUp = 64

// contentHashLimit caps which regular config files get content-folded (vs stat-only).
const contentHashLimit = 64 * 1024

// configNames are the per-directory toolchain files whose presence or content
// changes the activated env (mise/asdf/nvm/direnv/language pins).
var configNames = []string{
	"mise.toml",
	".mise.toml",
	".tool-versions",
	".nvmrc",
	".node-version",
	".python-version",
	".ruby-version",
	".envrc",
}

// fingerprint hashes the on-disk state that determines workDir's activated env,
// so any change (a file appearing, vanishing, or edited) invalidates the cache.
func (p *provider) fingerprint(workDir string) string {
	h := sha256.New()

	for _, path := range controlledPaths(workDir) {
		hashPath(h, path)
	}

	for _, dir := range installDirs() {
		hashChildren(h, dir)
	}

	return hex.EncodeToString(h.Sum(nil))
}

// hashPath folds a path's stat metadata (and small regular-file content) into h;
// an unreadable path folds its absence, itself a fingerprinted fact.
func hashPath(h io.Writer, path string) {
	_, _ = io.WriteString(h, path+"\x00")

	info, err := os.Lstat(path)
	if err != nil {
		_, _ = h.Write([]byte{'-'})

		return
	}

	writeMeta(h, info)

	if info.Mode().IsRegular() && info.Size() > 0 && info.Size() < contentHashLimit {
		if b, err := os.ReadFile(path); err == nil {
			_, _ = h.Write(b)
		}
	}
}

// hashChildren folds each direct child's name+stat — catches a new version
// landing under <dir>/<tool>/ that leaves <dir>'s own mtime untouched.
func hashChildren(h io.Writer, dir string) {
	_, _ = io.WriteString(h, dir+"\x00")

	entries, err := os.ReadDir(dir) // sorted by name → deterministic
	if err != nil {
		_, _ = h.Write([]byte{'-'})

		return
	}

	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}

		_, _ = io.WriteString(h, e.Name())

		writeMeta(h, info)
	}
}

func writeMeta(h io.Writer, info os.FileInfo) {
	var meta [16]byte

	binary.LittleEndian.PutUint64(meta[0:8], uint64(info.ModTime().UnixNano()))
	binary.LittleEndian.PutUint64(meta[8:16], uint64(info.Size()))

	_, _ = h.Write(meta[:])
}

// controlledPaths returns, in stable order, the paths whose stat/content feeds
// the fingerprint: the walk-up config chain, then global config + rc files and
// the manager state whose *own* mtime moves on change (trust store, direnv allow,
// nvm node dir — each gets a direct child on install/trust).
func controlledPaths(workDir string) []string {
	home, _ := coagenthome.UserHome()

	var paths []string

	for dir, i := workDir, 0; dir != "" && i < maxWalkUp; i++ {
		for _, name := range configNames {
			paths = append(paths, filepath.Join(dir, name))
		}

		if dir == home || dir == "/" {
			break
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}

		dir = parent
	}

	if home != "" {
		paths = append(paths,
			filepath.Join(home, ".config", "mise", "config.toml"),
			filepath.Join(home, ".local", "state", "mise", "trusted-configs"),
			filepath.Join(home, ".local", "share", "direnv", "allow"),
			filepath.Join(home, ".nvm", "versions", "node"),
			filepath.Join(home, ".bashrc"),
			filepath.Join(home, ".bash_profile"),
			filepath.Join(home, ".profile"),
		)
	}

	return paths
}

// installDirs are laid out as <dir>/<tool>/<version>: a new version bumps the
// <tool> subdir's mtime but not <dir>'s, so these are scanned one level deep.
func installDirs() []string {
	home, _ := coagenthome.UserHome()
	if home == "" {
		return nil
	}

	return []string{
		filepath.Join(home, ".local", "share", "mise", "installs"),
		filepath.Join(home, ".asdf", "installs"),
	}
}
