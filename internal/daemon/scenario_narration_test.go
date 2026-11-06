package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertNarratedOutputOrder(t *testing.T, h *harness, root int64, want ...string) {
	t.Helper()

	var contents []string
	for _, row := range h.outbox(root) {
		if !strings.HasPrefix(row.SourceKey, "message:") {
			continue
		}

		assert.Equal(t, "message", row.Type)
		assert.Regexp(t, `^message:[0-9]+:(reply|final)$`, row.SourceKey)
		contents = append(contents, row.Content)
	}

	require.Equal(t, want, contents)
}
