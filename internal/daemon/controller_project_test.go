package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func newProjectTestManager(t *testing.T) (*svc, Store, *sql.DB) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	store := NewStore(db)
	sessStore := sessionstore.NewStore(db)
	mgr := newSvc(
		&mockFactory{},
		store,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		subagent.NewStore(db),
		subagent.NewTransactions(db),
		nil,
		sessStore,
		nil,
		nil,
	)

	return mgr, store, db
}

func TestSanitizeProjectName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "ascii", in: "posts", want: "posts"},
		{name: "cyrillic", in: "посты", want: "посты"},
		{name: "cyrillic with space", in: "  мои посты  ", want: "мои посты"},
		{name: "long cyrillic 33 runes passes", in: string(make33Runes()), want: string(make33Runes())},
		{name: "parent traversal", in: "../x", wantErr: true},
		{name: "slash", in: "a/b", wantErr: true},
		{name: "backslash", in: `a\b`, wantErr: true},
		{name: "reserved separator", in: "sys:notes", wantErr: true},
		{name: "reserved system directory", in: "sys_coagent", wantErr: true},
		{name: "nul byte", in: "a\x00b", wantErr: true},
		{name: "hidden dot", in: ".hidden", wantErr: true},
		{name: "dot", in: ".", wantErr: true},
		{name: "dotdot", in: "..", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   ", wantErr: true},
		{name: "65 runes rejected", in: string(makeRunes(65)), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := projectpath.SanitizeName(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				assert.NotEmpty(t, err.Error(), "rejection must name a rule")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func makeRunes(n int) []rune {
	r := make([]rune, n)
	for i := range r {
		r[i] = 'я'
	}

	return r
}

func make33Runes() []rune { return makeRunes(33) }

func TestCreateProject_MkdirGetOrCreateIdempotent(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newProjectTestManager(t)
	root := t.TempDir()
	ctrl := newTestController(mgr, &config.Config{
		UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: root},
	}, nil, nil).ForManager("test")

	res, err := ctrl.CreateProject(ctx, controllerapi.ProjectCreateData{Name: "посты"})
	require.NoError(t, err)
	assert.Positive(t, res.ID)
	assert.Equal(t, "посты", res.Name)
	assert.Equal(t, filepath.Join(root, "посты"), res.Path)

	info, statErr := os.Stat(res.Path)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())

	// Repeat with the same name: same id, no error (get-or-create is idempotent).
	res2, err := ctrl.CreateProject(ctx, controllerapi.ProjectCreateData{Name: "посты"})
	require.NoError(t, err)
	assert.Equal(t, res.ID, res2.ID)
}

func TestCreateProject_RejectsBadName(t *testing.T) {
	mgr, _, _ := newProjectTestManager(t)
	ctrl := newTestController(mgr, &config.Config{
		UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: t.TempDir()},
	}, nil, nil).ForManager("test")

	_, err := ctrl.CreateProject(context.Background(), controllerapi.ProjectCreateData{Name: "../escape"})
	require.Error(t, err)
}

func TestCreateProject_SystemIdentityUsesReservedNameAndDirectory(t *testing.T) {
	ctx := context.Background()
	mgr, store, _ := newProjectTestManager(t)
	root := t.TempDir()
	mgr.systemProject = filepath.Join(root, controllerapi.CoagentSystemProjectDir)
	ctrl := newTestController(mgr, &config.Config{
		UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: root},
	}, nil, nil).ForManager(controllerapi.BuiltinCLIManagerID)

	res, err := ctrl.CreateProject(ctx, controllerapi.ProjectCreateData{
		Name:   controllerapi.CoagentSystemProjectName,
		System: true,
	})
	require.NoError(t, err)
	assert.Equal(t, controllerapi.CoagentSystemProjectName, res.Name)
	assert.Equal(t, filepath.Join(root, controllerapi.CoagentSystemProjectDir), res.Path)

	stored, err := store.GetProjectName(ctx, res.ID)
	require.NoError(t, err)
	assert.Equal(t, controllerapi.CoagentSystemProjectName, stored)
}

func TestCreateSession_SystemProjectRequiresExplicitIdentity(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newProjectTestManager(t)
	root := t.TempDir()
	mgr.systemProject = filepath.Join(root, controllerapi.CoagentSystemProjectDir)
	ctrl := newTestController(mgr, &config.Config{
		UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: root},
	}, nil, nil).ForManager(controllerapi.BuiltinCLIManagerID)

	project, err := ctrl.CreateProject(ctx, controllerapi.ProjectCreateData{
		Name:   controllerapi.CoagentSystemProjectName,
		System: true,
	})
	require.NoError(t, err)

	_, err = ctrl.CreateSession(ctx, controllerapi.SessionCreateData{WorkDir: project.Path})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved system project")

	_, err = ctrl.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir:       project.Path,
		SystemProject: controllerapi.CoagentSystemProjectName,
	})
	require.NoError(t, err)

	roguePath := filepath.Join(t.TempDir(), controllerapi.CoagentSystemProjectDir)
	require.NoError(t, os.MkdirAll(roguePath, 0o755))

	_, err = ctrl.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir:       roguePath,
		SystemProject: controllerapi.CoagentSystemProjectName,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical configuration directory")
}

func TestCreateSession_PersistsOnlyTheBoundManagerOwner(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newProjectTestManager(t)
	ctrl := newTestController(mgr, &config.Config{}, nil, nil).ForManager("alpha")
	workDir := t.TempDir()

	ownedID, err := ctrl.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir: workDir,
		Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "spoofed",
			"channel":                               "test",
		},
	})
	require.NoError(t, err)
	owned, err := mgr.sessionStore.GetSession(ctx, ownedID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", owned.Attributes[controllerapi.SessionAttributeManagerID])
	assert.Equal(t, "test", owned.Attributes["channel"])

	secondID, err := ctrl.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir: workDir,
		Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "spoofed",
		},
	})
	require.NoError(t, err)
	second, err := mgr.sessionStore.GetSession(ctx, secondID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", second.Attributes[controllerapi.SessionAttributeManagerID])
}

func TestResolveProjectsRoot(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()

	defaultRoot := filepath.Join(home, coagenthome.DirName, coagenthome.ProjectsDirName)

	assert.Equal(t, defaultRoot, projectpath.ResolveRoot(nil), "nil config → default, ~-expanded")
	assert.Equal(t, defaultRoot, projectpath.ResolveRoot(&config.UnifiedConfig{}), "empty → default")
	assert.Equal(
		t,
		filepath.Join(home, "notes"),
		projectpath.ResolveRoot(&config.UnifiedConfig{ProjectsRoot: "~/notes"}),
	)
	assert.Equal(
		t,
		"/srv/proj",
		projectpath.ResolveRoot(&config.UnifiedConfig{ProjectsRoot: "/srv/proj/"}),
		"trailing slash cleaned",
	)

	// A relative root must be absolutized so it matches the abs work_dir the store
	// records — otherwise the picker filter never matches anything.
	got := projectpath.ResolveRoot(&config.UnifiedConfig{ProjectsRoot: "relnotes"})
	assert.True(t, filepath.IsAbs(got), "relative root must be absolutized")
	assert.Equal(t, "relnotes", filepath.Base(got))
}

func TestListRecentProjects_ExcludesNestedProjects(t *testing.T) {
	ctx := context.Background()
	mgr, store, _ := newProjectTestManager(t)
	root := t.TempDir()

	direct, err := store.GetOrCreateProject(ctx, filepath.Join(root, "posts"))
	require.NoError(t, err)
	// A session /spawn-ed into a subdir of the root is not a /new project; listing
	// it would let a pick reconstruct root/<basename> = the wrong folder.
	nested, err := store.GetOrCreateProject(ctx, filepath.Join(root, "posts", "sub"))
	require.NoError(t, err)

	got, err := mgr.ListRecentProjects(ctx, root)
	require.NoError(t, err)

	ids := make([]int64, len(got))
	for i, p := range got {
		ids[i] = p.ID
	}

	assert.Contains(t, ids, direct)
	assert.NotContains(t, ids, nested, "only direct children of root are listed")
}

func TestListRecentProjects_ExcludesSystemProjects(t *testing.T) {
	ctx := context.Background()
	mgr, store, _ := newProjectTestManager(t)
	root := t.TempDir()

	userID, err := store.GetOrCreateProject(ctx, filepath.Join(root, "notes"))
	require.NoError(t, err)
	_, err = store.GetOrCreateSystemProject(
		ctx,
		filepath.Join(root, controllerapi.CoagentSystemProjectDir),
		controllerapi.CoagentSystemProjectName,
	)
	require.NoError(t, err)

	got, err := mgr.ListRecentProjects(ctx, root)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, userID, got[0].ID)
}

func TestListProjects_ReturnsIDNameWorkDir(t *testing.T) {
	ctx := context.Background()
	_, store, _ := newProjectTestManager(t)

	id, err := store.GetOrCreateProject(ctx, "/tmp/local-proj")
	require.NoError(t, err)

	rows, err := store.ListProjects(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, id, rows[0].ID)
	assert.Equal(t, "local-proj", rows[0].Name)
	assert.Equal(t, "/tmp/local-proj", rows[0].WorkDir)
}

func TestListRecentProjects_Ordering(t *testing.T) {
	ctx := context.Background()
	mgr, store, db := newProjectTestManager(t)
	root := t.TempDir()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	mkProject := func(name string) int64 {
		pid, err := store.GetOrCreateProject(ctx, filepath.Join(root, name))
		require.NoError(t, err)

		return pid
	}

	addSession := func(pid int64, updatedAt time.Time, killed bool) {
		rec, err := mgr.sessionStore.CreateSession(ctx, pid, "m", "", nil)
		require.NoError(t, err)

		if killed {
			_, err = db.ExecContext(ctx,
				`UPDATE sessions SET updated_at = ?, killed_at = ? WHERE id = ?`, updatedAt, updatedAt, rec.ID)
		} else {
			_, err = db.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, updatedAt, rec.ID)
		}

		require.NoError(t, err)
	}

	// Order of creation fixes ascending ids, which drives the id-desc tie-break.
	pNoSessA := mkProject("no-session-a")
	pNoSessB := mkProject("no-session-b") // higher id → sorts before A among nil-activity
	pNew := mkProject("new")
	pOld := mkProject("old")
	pKillExcl := mkProject("kill-excl")
	pAllKilled := mkProject("all-killed")

	addSession(pNew, base, false)
	addSession(pOld, base.Add(-20*time.Minute), false)
	// Newer session is killed → excluded; activity is the older non-killed one.
	addSession(pKillExcl, base.Add(10*time.Minute), true)
	addSession(pKillExcl, base.Add(-5*time.Minute), false)
	// Only-killed project → falls back to the newest killed session.
	addSession(pAllKilled, base.Add(-30*time.Minute), true)

	// A project outside the root must never appear.
	pOutside, err := store.GetOrCreateProject(ctx, filepath.Join(t.TempDir(), "outside"))
	require.NoError(t, err)
	addSession(pOutside, base.Add(100*time.Minute), false)

	got, err := mgr.ListRecentProjects(ctx, root)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, p := range got {
		gotIDs[i] = p.ID
	}

	want := []int64{pNoSessB, pNoSessA, pNew, pKillExcl, pOld, pAllKilled}
	assert.Equal(t, want, gotIDs)
	assert.NotContains(t, gotIDs, pOutside)

	// nil LastActivity for no-session projects; set for the rest.
	byID := make(map[int64]*time.Time, len(got))
	for _, p := range got {
		byID[p.ID] = p.LastActivity
	}

	assert.Nil(t, byID[pNoSessA])
	assert.Nil(t, byID[pNoSessB])
	require.NotNil(t, byID[pKillExcl])
	assert.True(t, byID[pKillExcl].Equal(base.Add(-5*time.Minute)), "killed session must be excluded")
	require.NotNil(t, byID[pAllKilled])
	assert.True(t, byID[pAllKilled].Equal(base.Add(-30*time.Minute)), "all-killed falls back to newest killed")
}
