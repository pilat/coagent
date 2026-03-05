package shellenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

func TestControlledPaths_WalkUpAndGlobals(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()

	set := make(map[string]bool)
	for _, p := range controlledPaths("/x/y/z") {
		set[p] = true
	}

	assert.True(t, set[filepath.Join("/x/y/z", "mise.toml")], "workdir config")
	assert.True(t, set[filepath.Join("/x/y", ".tool-versions")], "parent config (walk-up)")
	assert.True(t, set[filepath.Join("/x", ".envrc")], "grandparent config (walk-up)")

	assert.True(t, set[filepath.Join(home, ".local", "state", "mise", "trusted-configs")], "mise trust store")
	assert.True(t, set[filepath.Join(home, ".nvm", "versions", "node")], "nvm node dir")
	assert.True(t, set[filepath.Join(home, ".bashrc")], "rc file")
}

func TestHashChildren_DetectsChildMtimeChange(t *testing.T) {
	installs := t.TempDir()
	tool := filepath.Join(installs, "go")
	require.NoError(t, os.Mkdir(tool, 0o700))

	sum := func() string {
		h := sha256.New()
		hashChildren(h, installs)

		return hex.EncodeToString(h.Sum(nil))
	}

	before := sum()

	// A new version landing under installs/go/ bumps go/'s mtime but not installs/'s.
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(tool, future, future))

	assert.NotEqual(t, before, sum(), "a child dir's mtime change must be caught by the shallow scan")
}

func TestFingerprint_ReflectsConfigChanges(t *testing.T) {
	dir := t.TempDir()

	var p provider

	base := p.fingerprint(dir)
	assert.Equal(t, base, p.fingerprint(dir), "stable while nothing changes")

	cfg := filepath.Join(dir, "mise.toml")
	require.NoError(t, os.WriteFile(cfg, []byte("[tools]\ngo = \"1.26.5\"\n"), 0o600))
	created := p.fingerprint(dir)
	assert.NotEqual(t, base, created, "a config appearing must change the fingerprint")

	require.NoError(t, os.WriteFile(cfg, []byte("[tools]\ngo = \"1.26.1\"\n"), 0o600))
	assert.NotEqual(t, created, p.fingerprint(dir), "editing a config must change the fingerprint")

	require.NoError(t, os.Remove(cfg))
	assert.NotEqual(t, created, p.fingerprint(dir), "a config vanishing must change the fingerprint")
}

func TestFingerprint_UnavailableIsEmpty(t *testing.T) {
	var p provider // shell == ""

	assert.Empty(t, p.Fingerprint("/some/dir"))

	p.shell = "/bin/bash"
	assert.Empty(t, p.Fingerprint(""))
}

func newCountingProvider(t *testing.T, captures *int) *provider {
	t.Helper()

	p := &provider{
		shell:    "/bin/bash",
		salt:     []byte("fp-test-salt"),
		ttl:      30 * time.Minute,
		cacheDir: t.TempDir(),
	}
	p.captureFn = func(context.Context, string) ([]byte, error) {
		*captures++

		return []byte("declare -x PATH=\"/bin\"\n"), nil
	}

	return p
}

func TestSnapshot_RecapturesOnFingerprintChange(t *testing.T) {
	work := t.TempDir()

	var captures int

	p := newCountingProvider(t, &captures)

	require.NotEmpty(t, p.Snapshot(context.Background(), work))
	require.Equal(t, 1, captures)

	// Unchanged env → served from cache, no recapture.
	require.NotEmpty(t, p.Snapshot(context.Background(), work))
	require.Equal(t, 1, captures, "unchanged env must not recapture")

	// A toolchain config appears → fingerprint changes → recapture on next spawn.
	require.NoError(t, os.WriteFile(filepath.Join(work, ".tool-versions"), []byte("go 1.26.5\n"), 0o600))
	require.NotEmpty(t, p.Snapshot(context.Background(), work))
	require.Equal(t, 2, captures, "a config change must recapture")
}

func TestSnapshot_InvalidateForcesRecapture(t *testing.T) {
	work := t.TempDir()

	var captures int

	p := newCountingProvider(t, &captures)

	require.NotEmpty(t, p.Snapshot(context.Background(), work))
	require.Equal(t, 1, captures)

	p.Invalidate(work)

	require.NotEmpty(t, p.Snapshot(context.Background(), work))
	require.Equal(t, 2, captures, "Invalidate must force a recapture")
}
