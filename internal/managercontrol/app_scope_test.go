package managercontrol

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestAppScope(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	in := f.root(t, f.p, nil)
	out := f.root(t, f.q, nil)
	worktree := f.root(t, "/worktrees/attested", map[string]any{
		"repo_root": f.p, controllerapi.SessionAttributeWorktreeOrigin: controllerapi.WorktreeOriginController,
	})
	unattested := f.root(t, "/worktrees/unattested", map[string]any{"repo_root": f.p})
	outside, err := f.store.GetSession(t.Context(), out)
	require.NoError(t, err)
	child, err := f.store.CreateSubagentSession(t.Context(), outside.ProjectID, out, out, "general", "model", "")
	require.NoError(t, err)
	app := f.control(t, []string{f.p})
	calls := map[string]func(int64) error{
		"SendSessionMessage": func(id int64) error {
			return app.SendSessionMessage(
				t.Context(),
				controllerapi.SessionMessageData{SessionID: id, Message: "hello"},
			)
		},
		"StopSession": func(id int64) error {
			_, err := app.StopSession(t.Context(), controllerapi.SessionStopData{SessionID: id})
			return err
		},
		"KillSession": func(id int64) error {
			_, err := app.KillSession(t.Context(), controllerapi.SessionKillData{SessionID: id})
			return err
		},
		"TurnOver": func(id int64) error {
			_, _, err := app.TurnOver(t.Context(), id)
			return err
		},
		"ChatOutputs": func(id int64) error {
			_, err := app.ChatOutputs(t.Context(), "", id, 0, 10)
			return err
		},
		"ChatEntry": func(id int64) error {
			_, _, err := app.ChatEntry(t.Context(), "", id, 10)
			return err
		},
		"SessionKilled": func(id int64) error {
			_, err := app.SessionKilled(t.Context(), id)
			return err
		},
		"Successor": func(id int64) error {
			_, err := app.Successor(t.Context(), id)
			return err
		},
		"Orchestrator": func(id int64) error {
			_, err := app.Orchestrator(t.Context(), "", id)
			return err
		},
		"ListSkills": func(id int64) error {
			_, err := app.ListSkills(t.Context(), controllerapi.ConfigSkillsData{SessionID: id})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			require.EqualError(t, call(out), fmt.Sprintf("session %d not found", out))
			require.EqualError(t, call(99999), "session 99999 not found")
			require.EqualError(t, call(unattested), fmt.Sprintf("session %d not found", unattested))
			require.EqualError(t, call(child), fmt.Sprintf("session %d not found", child))
			require.NoError(t, call(in))
			require.NoError(t, call(worktree))
		})
	}
	_, _, err = app.TurnOver(t.Context(), 0)
	require.EqualError(t, err, "#0 is not a live session")

	_, err = app.CreateSession(t.Context(), controllerapi.SessionCreateData{WorkDir: f.q})
	require.EqualError(t, err, fmt.Sprintf("no project at %q", f.q))
	id, err := app.CreateSession(t.Context(), controllerapi.SessionCreateData{WorkDir: f.p + "/../p"})
	require.NoError(t, err)
	assert.Equal(t, int64(77), id)
	assert.Equal(t, f.p, f.backend.calls[len(f.backend.calls)-1].text)
	for _, test := range []struct {
		name    string
		control appTestControl
		want    []int64
	}{
		{name: "scoped", control: app, want: []int64{in, worktree}},
		{name: "unscoped", control: f.control(t, nil), want: []int64{in, out, worktree, unattested}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := test.control.ListSessions(t.Context())
			require.NoError(t, err)
			var ids []int64
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			assert.ElementsMatch(t, test.want, ids)
			chats, err := test.control.ChatSessions(t.Context(), "")
			require.NoError(t, err)
			ids = nil
			for _, row := range chats {
				ids = append(ids, row.ID)
			}
			assert.ElementsMatch(t, test.want, ids)
			projects, err := test.control.ListRecentProjects(t.Context())
			require.NoError(t, err)
			paths, err := test.control.SpawnFavorites(t.Context())
			require.NoError(t, err)
			wantPaths := []string{f.p}
			if test.name == "unscoped" {
				wantPaths = append(wantPaths, f.q)
			}
			assert.ElementsMatch(t, wantPaths, paths)
			paths = nil
			for _, project := range projects.Projects {
				paths = append(paths, project.Path)
			}
			assert.ElementsMatch(t, wantPaths, paths)
		})
	}
	_, err = f.control(t, nil).SessionKilled(t.Context(), out)
	require.NoError(t, err)
}

func TestScopedAppRefusesSendToAnOrchestrator(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	on := f.root(t, f.p, map[string]any{controllerapi.SessionAttributeOrchestrator: true})
	off := f.root(t, f.p, nil)
	app := f.control(t, []string{f.p})
	send := func(control appTestControl, id int64) error {
		return control.SendSessionMessage(t.Context(), controllerapi.SessionMessageData{
			SessionID: id, Message: "hello", IdempotencyKey: "k1",
		})
	}
	require.EqualError(t, send(app, on), fmt.Sprintf(
		"orchestrator mode is on for session #%d; a project-scoped app cannot send to it", on))
	next, _, err := f.store.ReplaceManagerRoot(t.Context(), off, "p", f.p)
	require.NoError(t, err)
	_, err = f.store.SetOrchestrator(t.Context(), next.ID, true)
	require.NoError(t, err)
	require.EqualError(t, send(app, off), fmt.Sprintf(
		"orchestrator mode is on for session #%d; a project-scoped app cannot send to it", next.ID))
	assert.Empty(t, f.backend.calls)
	require.NoError(t, send(f.control(t, nil), off))
	assert.Equal(
		t,
		[]appBackendCall{{verb: "send", app: "bot", id: off, text: "hello", key: "bot:k1"}},
		f.backend.calls,
	)
}
