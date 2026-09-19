//go:build !linux

package safefile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectConfinedFailsClosedWithoutPlatformIdentity pins the generic
// non-Linux fallback: with no platform root identity, a shielded open must
// refuse rather than silently run unconfined. The Linux-only runtime guard is
// the product boundary that refuses such binaries earlier.
func TestProjectConfinedFailsClosedWithoutPlatformIdentity(t *testing.T) {
	_, err := New(t.TempDir(), ProjectConfined)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project root identity is unavailable on this platform")
}
