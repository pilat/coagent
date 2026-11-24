package managercontrol

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

type stubWorktreeBackend struct {
	Backend
	Store
	sendErr       error
	workDir       string
	sentProjectID int64
	sentWorktree  string
	sentPrompt    string
	sentAttrs     map[string]any
	session       *sessionstore.SessionRecord
}

func (f *stubWorktreeBackend) CreateSession(
	_ context.Context, projectID int64, worktree, prompt, _ string, attrs map[string]any,
) (int64, error) {
	f.sentProjectID = projectID
	f.sentWorktree = worktree
	f.sentPrompt = prompt
	f.sentAttrs = attrs
	if f.sendErr != nil {
		return 0, f.sendErr
	}

	return 77, nil
}

func (f *stubWorktreeBackend) GetOrCreateProject(_ context.Context, workDir string) (int64, error) {
	f.workDir = workDir

	return 9, nil
}

func (f *stubWorktreeBackend) SendToSessionResolved(context.Context, int64, string, string) (int64, error) {
	return 0, nil
}

func (f *stubWorktreeBackend) GetSession(context.Context, int64) (*sessionstore.SessionRecord, error) {
	return f.session, nil
}

func (f *stubWorktreeBackend) SetModel(context.Context, int64, string, string) error { return nil }

func (f *stubWorktreeBackend) HasActiveLoop(int64) bool { return false }

func (f *stubWorktreeBackend) NotifySession(int64, sessionevent.Notification) {}

func worktreeSessionService(t *testing.T, backend *stubWorktreeBackend) *service {
	t.Helper()

	return &service{backend: backend, store: backend}
}

func TestCreateSession_HandsTheWorktreeToTheBackend(t *testing.T) {
	backend := &stubWorktreeBackend{}
	s := worktreeSessionService(t, backend)
	data := controllerapi.SessionCreateData{
		WorkDir: "/repo/work", WorktreeName: "api", Attributes: map[string]any{"channel": "telegram"},
	}
	id, err := s.createSession(t.Context(), "telegram", data)
	require.NoError(t, err)
	assert.Equal(t, int64(77), id)
	assert.Equal(t, int64(9), backend.sentProjectID)
	assert.Equal(t, "/repo/work", backend.workDir)
	assert.Equal(t, "api", backend.sentWorktree)
	assert.Empty(t, backend.sentPrompt)
	assert.NotContains(t, backend.sentAttrs, "manager_id")
	assert.Equal(t, "telegram", backend.sentAttrs["channel"])
	assert.NotContains(t, backend.sentAttrs, "repo_root")
	assert.NotContains(t, backend.sentAttrs, controllerapi.SessionAttributeWorktreeOrigin)
	assert.NotContains(t, data.Attributes, "manager_id")
	backend.sendErr = errors.New(`worktree: branch "api" already exists`)
	id, err = s.createSession(t.Context(), "telegram", data)
	require.EqualError(t, err, `worktree: branch "api" already exists`)
	assert.Zero(t, id)
}

func TestCreateSession_RejectsCallerSuppliedRepoRoot(t *testing.T) {
	backend := &stubWorktreeBackend{}
	s := worktreeSessionService(t, backend)
	for _, data := range []controllerapi.SessionCreateData{
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
