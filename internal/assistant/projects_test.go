package assistant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantProjectsFiltersAndKeys(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, sessions, s, root := assistantFixture(t)
	scope := Scope{Root: root, Owner: testOwner}
	_, err := sessions.GetOrCreateHiddenProject(ctx, t.TempDir())
	require.NoError(t, err)
	worktrees, err := coagenthome.Join(coagenthome.WorktreesDirName)
	require.NoError(t, err)
	worktree := filepath.Join(worktrees, "catalog-hidden")
	require.NoError(t, os.MkdirAll(worktree, 0o700))
	_, err = sessions.GetOrCreateProject(ctx, worktree)
	require.NoError(t, err)
	_, err = sessions.GetOrCreateProject(ctx, filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, err)
	projectsRoot, err := coagenthome.Join(coagenthome.ProjectsDirName)
	require.NoError(t, err)
	scratch := filepath.Join(projectsRoot, "catalog-same")
	require.NoError(t, os.MkdirAll(scratch, 0o700))
	first, err := sessions.GetOrCreateNamedProject(ctx, t.TempDir(), "same")
	require.NoError(t, err)
	second, err := sessions.GetOrCreateNamedProject(ctx, scratch, "same")
	require.NoError(t, err)
	work, err := sessions.CreateSession(ctx, first, "model", "", map[string]any{"manager_id": testOwner})
	require.NoError(t, err)
	_, err = sessions.Enqueue(ctx, sessionstore.Input{
		SessionID: work.ID, Source: sessionstore.InputSourceUser, Content: "work",
		Attributes: map[string]any{"from": "assistant", sessionstore.ObserverSessionIDAttribute: root},
	})
	require.NoError(t, err)

	projects, err := s.Projects(ctx)
	require.NoError(t, err)
	for _, project := range projects {
		assert.NotContains(t, []string{"catalog-hidden", "missing"}, project.Name)
		if project.Name != "same" {
			continue
		}

		assert.NotEqual(t, "same", project.Key)
		choice, err := s.resolveProject(ctx, project.Key)
		require.NoError(t, err)
		assert.Contains(t, []int64{first, second}, choice.id)
		if choice.id == second {
			assert.True(t, strings.HasPrefix(project.Place, "scratch "))
		}
	}
	listed, _, err := s.List(ctx, scope, 0, 20, catalogPath(scratch))
	require.NoError(t, err)
	assert.Empty(t, listed)
	_, _, err = s.List(ctx, scope, 0, 20, "nowhere")
	require.ErrorContains(t, err, `no project "nowhere"`)

	_, err = s.lookupProject(ctx, "fresh-research")
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(s.projectsRoot, "fresh-research"))
	assert.True(t, os.IsNotExist(err), "validating an open creates nothing")
	_, err = s.lookupProject(ctx, "../outside")
	require.Error(t, err)
}

func TestAssistantProjectPathArg(t *testing.T) {
	t.Parallel()
	home, err := coagenthome.UserHome()
	require.NoError(t, err)
	path, ok := projectPathArg("~/projects/pilat/coagent")
	assert.True(t, ok)
	assert.Equal(t, filepath.Join(home, "projects/pilat/coagent"), path)
	path, ok = projectPathArg("/srv/repo/")
	assert.True(t, ok)
	assert.Equal(t, "/srv/repo", path)
	_, ok = projectPathArg("coagent")
	assert.False(t, ok)
}
