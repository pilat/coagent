package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var isolatedHomeKeys = []string{
	"HOME",
	"USERPROFILE",
	"XDG_CACHE_HOME",
	"XDG_CONFIG_HOME",
	"XDG_DATA_HOME",
	"XDG_STATE_HOME",
}

// isolatedProcessEnv preserves ordinary process configuration while replacing
// every OS/user-directory hint that could lead a subprocess back into the
// developer's profile. Callers must pass a test-owned home.
func isolatedProcessEnv(environ []string, home string) []string {
	replaced := make(map[string]struct{}, len(isolatedHomeKeys))
	for _, key := range isolatedHomeKeys {
		replaced[key] = struct{}{}
	}

	result := make([]string, 0, len(environ)+len(isolatedHomeKeys))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if _, remove := replaced[key]; ok && remove {
			continue
		}

		result = append(result, entry)
	}

	values := map[string]string{
		"HOME":            home,
		"USERPROFILE":     home,
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
	}
	for _, key := range isolatedHomeKeys {
		result = append(result, key+"="+values[key])
	}

	return result
}

func TestIsolatedProcessEnv_ReplacesEveryUserDirectoryHint(t *testing.T) {
	home := t.TempDir()
	environ := isolatedProcessEnv([]string{
		"PATH=/usr/bin",
		"HOME=/real/home",
		"HOME=/another/home",
		"USERPROFILE=/real/profile",
		"XDG_CACHE_HOME=/real/cache",
		"XDG_CONFIG_HOME=/real/config",
		"XDG_DATA_HOME=/real/data",
		"XDG_STATE_HOME=/real/state",
	}, home)

	assert.Contains(t, environ, "PATH=/usr/bin")
	for _, key := range isolatedHomeKeys {
		var values []string
		for _, entry := range environ {
			entryKey, value, ok := strings.Cut(entry, "=")
			if ok && entryKey == key {
				values = append(values, value)
			}
		}

		require.Len(t, values, 1, "%s must occur exactly once", key)
		rel, err := filepath.Rel(home, values[0])
		require.NoError(t, err)
		assert.False(t, rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)),
			"%s escaped isolated home: %s", key, values[0])
	}
}
