package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

// controllerapi mirrors the outbox type vocabulary as wire literals because it
// must not import sessionstore; this test is the pin that keeps them identical.
func TestOutputTypeVocabularyMatchesStore(t *testing.T) {
	assert.Equal(t, controllerapi.OutputMessageReplaceable, string(sessionstore.OutputMessageReplaceable))
	assert.Equal(t, controllerapi.OutputMessagePersistent, string(sessionstore.OutputMessagePersistent))
	assert.Equal(t, controllerapi.OutputSessionOpened, string(sessionstore.OutputSessionOpened))
	assert.Equal(t, controllerapi.OutputSessionReplaced, string(sessionstore.OutputSessionReplaced))
	assert.Equal(t, controllerapi.OutputSessionClosed, string(sessionstore.OutputSessionClosed))
}

func TestStore_GetOrCreateProject(t *testing.T) {
	t.Run("creates and returns stable ID", func(t *testing.T) {
		s := newTestStore(t)

		id1, err := s.GetOrCreateProject(context.Background(), "/tmp/project")
		require.NoError(t, err)
		assert.Positive(t, id1)

		id2, err := s.GetOrCreateProject(context.Background(), "/tmp/project")
		require.NoError(t, err)
		assert.Equal(t, id1, id2, "same workdir should return same project ID")
	})

	t.Run("different workdirs get different IDs", func(t *testing.T) {
		s := newTestStore(t)

		id1, err := s.GetOrCreateProject(context.Background(), "/tmp/a")
		require.NoError(t, err)
		id2, err := s.GetOrCreateProject(context.Background(), "/tmp/b")
		require.NoError(t, err)
		assert.NotEqual(t, id1, id2)
	})

	t.Run("GetProjectWorkDir reverse lookup", func(t *testing.T) {
		s := newTestStore(t)

		pid, err := s.GetOrCreateProject(context.Background(), "/tmp/project")
		require.NoError(t, err)

		workDir, err := s.GetProjectWorkDir(context.Background(), pid)
		require.NoError(t, err)
		assert.Contains(t, workDir, "/tmp/project")
	})

	t.Run("rejects reserved system directory", func(t *testing.T) {
		s := newTestStore(t)

		_, err := s.GetOrCreateProject(context.Background(), "/tmp/sys_coagent")
		require.Error(t, err)
	})
}
