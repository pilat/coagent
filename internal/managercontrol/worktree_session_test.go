package managercontrol

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// stubWorktreeBackend serves createSession's needs only: Send and
// GetOrCreateNamedProject record their calls, everything else is unused.
type stubWorktreeBackend struct {
	sendErr       error
	namedProject  string
	sentProjectID int64
}

func (f *stubWorktreeBackend) Send(_ context.Context, projectID int64, _, _ string, _ map[string]any) (int64, error) {
	if f.sendErr != nil {
		return 0, f.sendErr
	}

	f.sentProjectID = projectID

	return 77, nil
}

func (f *stubWorktreeBackend) GetOrCreateNamedProject(_ context.Context, _, name string) (int64, error) {
	f.namedProject = name

	return 5, nil
}

func (f *stubWorktreeBackend) SendToSessionResolved(context.Context, int64, string) (int64, error) {
	return 0, nil
}

func (f *stubWorktreeBackend) GetSession(context.Context, int64) (*sessionstore.SessionRecord, error) {
	return nil, nil
}

func (f *stubWorktreeBackend) List(context.Context) ([]*sessionstore.SessionRecord, error) {
	return nil, nil
}

func (f *stubWorktreeBackend) SetModel(context.Context, int64, string, string) error { return nil }

func (f *stubWorktreeBackend) SetAttributes(context.Context, int64, map[string]any) error {
	return nil
}

func (f *stubWorktreeBackend) GetOrCreateProject(context.Context, string) (int64, error) {
	return 0, nil
}

func (f *stubWorktreeBackend) GetOrCreateSystemProject(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (f *stubWorktreeBackend) GetProjectWorkDir(context.Context, int64) (string, error) {
	return "", nil
}

func (f *stubWorktreeBackend) GetProjectName(context.Context, int64) (string, error) {
	return "", nil
}

func (f *stubWorktreeBackend) HasActiveLoop(int64) bool { return false }

func (f *stubWorktreeBackend) PubSub() sessionbus.Source { return nil }

func (f *stubWorktreeBackend) NotifySession(int64, sessionevent.Notification) {}

func (f *stubWorktreeBackend) CurrentProgress(context.Context, int64) (*controllerapi.ProgressData, error) {
	return nil, nil
}

func (f *stubWorktreeBackend) RefreshProgress(context.Context, int64) error { return nil }

func (f *stubWorktreeBackend) ReconcileOutputReadiness(context.Context, int64) error {
	return nil
}

func worktreeSessionService(t *testing.T, backend Backend) *service {
	t.Helper()

	return &service{backend: backend, cfg: &config.Config{
		UnifiedConfig: &config.UnifiedConfig{WorktreesRoot: t.TempDir()},
	}}
}

func TestCreateSession_WorktreeRollsBackWhenLaunchFails(t *testing.T) {
	clone := setupClone(t)
	backend := &stubWorktreeBackend{sendErr: errors.New("boom")}
	s := worktreeSessionService(t, backend)

	sessionID, err := s.createSession(context.Background(), "telegram", controllerapi.SessionCreateData{
		WorkDir:      clone,
		WorktreeName: "api",
	})
	require.Error(t, err)
	assert.Zero(t, sessionID)

	client := git.NewWorktreeClient()
	ctx := context.Background()
	repoRoot, err := client.RepoRoot(ctx, clone)
	require.NoError(t, err)

	assert.NoDirExists(t, projectpath.WorktreePath(
		projectpath.ResolveWorktreesRoot(s.unifiedConfig()), repoRoot, "api",
	), "the worktree must roll back when launch fails after project registration")

	exists, err := client.BranchExists(ctx, clone, "api")
	require.NoError(t, err)
	assert.False(t, exists, "the branch must roll back too")
}

func TestCreateSession_WorktreeHappyPath(t *testing.T) {
	clone := setupClone(t)
	backend := &stubWorktreeBackend{}
	s := worktreeSessionService(t, backend)

	sessionID, err := s.createSession(context.Background(), "telegram", controllerapi.SessionCreateData{
		WorkDir:      clone,
		WorktreeName: "api",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(77), sessionID)

	assert.Equal(t, int64(5), backend.sentProjectID, "Send must target the named worktree project")
	assert.Equal(t, projectpath.RepoNamespace(clone)+"/api", backend.namedProject,
		"the worktree project must register under its <repo>/<branch> display name")

	client := git.NewWorktreeClient()
	repoRoot, err := client.RepoRoot(context.Background(), clone)
	require.NoError(t, err)
	assert.DirExists(t, projectpath.WorktreePath(
		projectpath.ResolveWorktreesRoot(s.unifiedConfig()), repoRoot, "api",
	))
}
