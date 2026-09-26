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
	sentAttrs     map[string]any
	session       *sessionstore.SessionRecord
}

func (f *stubWorktreeBackend) Send(
	_ context.Context, projectID int64, _, _ string, attrs map[string]any,
) (int64, error) {
	if f.sendErr != nil {
		return 0, f.sendErr
	}

	f.sentProjectID = projectID
	f.sentAttrs = attrs

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
	return f.session, nil
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

func (f *stubWorktreeBackend) GetOrCreateHiddenProject(context.Context, string) (int64, error) {
	return 0, nil
}

func (f *stubWorktreeBackend) EnsureManagementRoot(
	context.Context, int64, string, int64, string, string,
) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error) {
	return nil, nil, nil
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
	assert.Equal(t, repoRoot, backend.sentAttrs["repo_root"])
	assert.Equal(t, controllerapi.WorktreeOriginController,
		backend.sentAttrs[controllerapi.SessionAttributeWorktreeOrigin])
}

func TestCreateSession_RejectsCallerSuppliedRepoRoot(t *testing.T) {
	backend := &stubWorktreeBackend{}
	s := worktreeSessionService(t, backend)
	for _, data := range []controllerapi.SessionCreateData{
		{WorkDir: t.TempDir(), RepoRoot: "/elsewhere"},
		{WorkDir: t.TempDir(), Attributes: map[string]any{"repo_root": "/elsewhere"}},
		{WorkDir: t.TempDir(), Attributes: map[string]any{
			controllerapi.SessionAttributeWorktreeOrigin: controllerapi.WorktreeOriginController,
		}},
	} {
		_, err := s.createSession(t.Context(), "telegram", data)
		require.ErrorContains(t, err, "reserved for created worktrees")
	}
	assert.Nil(t, backend.sentAttrs)
}

func TestSetSessionAttributes_PreservesWorktreeRepoRoot(t *testing.T) {
	backend := &stubWorktreeBackend{session: &sessionstore.SessionRecord{
		Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID:      "telegram",
			controllerapi.SessionAttributeWorktreeOrigin: controllerapi.WorktreeOriginController,
			"repo_root": "/source",
		},
	}}
	s := worktreeSessionService(t, backend)
	data := controllerapi.SessionSetAttributesData{SessionID: 7, Attributes: map[string]any{"topic": 1}}
	require.NoError(t, s.authorizeAttributeUpdate(t.Context(), "telegram", &data))
	assert.Equal(t, "/source", data.Attributes["repo_root"])
	assert.Equal(t, controllerapi.WorktreeOriginController,
		data.Attributes[controllerapi.SessionAttributeWorktreeOrigin])

	data.Attributes["repo_root"] = "/other"
	require.ErrorContains(t, s.authorizeAttributeUpdate(t.Context(), "telegram", &data), "reserved")
	delete(data.Attributes, "repo_root")
	data.Attributes[controllerapi.SessionAttributeWorktreeOrigin] = "spoofed"
	require.ErrorContains(t, s.authorizeAttributeUpdate(t.Context(), "telegram", &data), "reserved")
}
