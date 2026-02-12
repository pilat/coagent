package coagenthome

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirUnderOverriddenHome(t *testing.T) {
	restore := Override("/fake/home")
	defer restore()

	dir, err := Dir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/fake/home", DirName), dir)
}

func TestJoin(t *testing.T) {
	restore := Override("/fake/home")
	defer restore()

	got, err := Join(CacheDirName, CatalogDirName)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/fake/home", DirName, CacheDirName, CatalogDirName), got)

	got, err = Join()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/fake/home", DirName), got)
}

func TestUserHomeConcurrentWithOverride(t *testing.T) {
	var wg sync.WaitGroup

	wg.Go(func() {
		restore := Override("/fake/home")
		restore()
	})

	for range 20 {
		wg.Go(func() {
			_, _ = UserHome()
		})
	}

	wg.Wait()
}

func TestOverrideEmptySimulatesUnresolvableHome(t *testing.T) {
	restore := Override("")
	defer restore()

	_, err := UserHome()
	require.Error(t, err)

	_, err = Dir()
	require.Error(t, err)

	_, err = Join(CacheDirName)
	require.Error(t, err)
}

func TestOverrideRestore(t *testing.T) {
	t.Setenv("HOME", "/env/home")

	restore := Override("/fake/home")
	got, err := UserHome()
	require.NoError(t, err)
	assert.Equal(t, "/fake/home", got)

	restore()
	got, err = UserHome()
	require.NoError(t, err)
	assert.Equal(t, "/env/home", got)
}

func TestUserHomeRejectsInheritedHomeInTestBinary(t *testing.T) {
	if startupUserHome == "" {
		t.Skip("process started without a resolvable home")
	}

	t.Setenv("HOME", startupUserHome)

	_, err := UserHome()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to use the inherited user home")
}

func TestUserHomeAllowsIsolatedEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := UserHome()
	require.NoError(t, err)
	assert.Equal(t, home, got)
}

func TestUserHomeRejectsSymlinkToInheritedHome(t *testing.T) {
	if startupUserHome == "" {
		t.Skip("process started without a resolvable home")
	}

	alias := filepath.Join(t.TempDir(), "home-alias")
	if err := os.Symlink(startupUserHome, alias); err != nil {
		t.Skipf("cannot create home symlink: %v", err)
	}
	t.Setenv("HOME", alias)

	_, err := UserHome()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to use the inherited user home")
}

func TestUserHomeRejectsPathBeneathInheritedHome(t *testing.T) {
	if startupUserHome == "" || filepath.Dir(startupUserHome) == startupUserHome {
		t.Skip("process started without a non-root home")
	}

	t.Setenv("HOME", filepath.Join(startupUserHome, "test-home"))

	_, err := UserHome()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path beneath it")
}

func TestUserHomeRejectsOverrideToInheritedHome(t *testing.T) {
	if startupUserHome == "" {
		t.Skip("process started without a resolvable home")
	}

	restore := Override(startupUserHome)
	defer restore()

	_, err := UserHome()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to use the inherited user home")
}
