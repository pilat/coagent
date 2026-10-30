package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

var (
	_                 Host = (*toolHost)(nil)
	updateToolSurface      = flag.Bool("update", false, "update the assistant tool surface golden")
)

type toolHost struct {
	sessions   *sessionstore.Store
	attributes map[string]any
	deliveries []string
	created    []int64
	stopped    []int64
	focused    []int64
	models     []Model
	modelsRead int
	modelCalls []modelArgs
}

type toolFixture struct {
	db         *sql.DB
	sessions   *sessionstore.Store
	store      *Store
	root, work int64
	host       *toolHost
	tools      map[string]tool.Tool
}

func TestAssistantToolSurfaceUnchanged(t *testing.T) {
	t.Parallel()
	type surface struct {
		ID           string          `json:"id"`
		Description  string          `json:"description"`
		Parameters   json.RawMessage `json:"parameters"`
		ParallelSafe bool            `json:"parallel_safe"`
	}

	var entries []surface
	for _, instance := range Tools(nil, Scope{}, nil) {
		entries = append(entries, surface{
			ID: instance.ID(), Description: instance.Description(),
			Parameters: instance.Parameters(), ParallelSafe: instance.ParallelSafe(),
		})
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	require.NoError(t, err)
	data = append(data, '\n')
	path := filepath.Join("testdata", "tool_surface.golden.json")
	if *updateToolSurface {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
	golden, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(golden), string(data))
}

func TestAssistantToolsImmediateEffects(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	assert.Contains(t, f.execute(t, "read_session", `{"session":`+itoa(f.work)+`,"count":2}`), "tail ok")
	assert.Contains(t, f.execute(t, "sessions", `{"offset":0}`), itoa(f.work))
	assert.Contains(t, f.execute(t, "projects", `{}`), "devbox")
	assert.Equal(t, "focus is empty", f.execute(t, "focus_list", `{}`))
	memory := f.execute(t, "remember", `{"text":"searchable fact"}`)
	assert.Regexp(t, `^m[0-9]+$`, memory)
	assert.Contains(t, f.execute(t, "search", `{"query":"searchable"}`), memory)
	f.execute(t, "rewrite_memory", `{"id":"`+memory+`","text":"corrected fact"}`)
	notes, err := f.store.Memory(t.Context(), f.root)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "corrected fact", notes[0].Text)
	f.execute(t, "forget_memory", `{"id":"`+memory+`"}`)
	assert.Contains(t, f.execute(t, "search", `{"query":"corrected"}`), "nothing matches")
	focus := f.execute(t, "focus", `{"text":"waiting","sessions":["`+itoa(f.work)+`",`+itoa(f.work)+`]}`)
	assert.Regexp(t, `^f[0-9]+$`, focus)
	f.execute(t, "update_focus", `{"id":"`+focus+`","text":"still waiting"}`)
	items, err := f.store.Focus(t.Context(), f.root)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, []string{itoa(f.work)}, items[0].Sessions, "omitting sessions preserves the wait")
	assert.Contains(t, f.execute(t, "focus_list", `{}`), "still waiting → "+itoa(f.work))
	f.execute(t, "update_focus", `{"id":"`+focus+`","text":"finished","sessions":[]}`)
	items, err = f.store.Focus(t.Context(), f.root)
	require.NoError(t, err)
	assert.Empty(t, items[0].Sessions)
	f.execute(t, "done_focus", `{"id":"`+focus+`"}`)
	assert.Equal(t, "focus is empty", f.execute(t, "focus_list", `{}`))
	_, final := answered(t, f.sessions, f.work, "other request", "answer with context")
	assert.Contains(t, f.execute(t, "around", fmt.Sprintf(`{"ref":"msg %d","count":2}`, final)), "other request")
	f.execute(t, "send", `{"session":"`+itoa(f.work)+`","text":"continue"}`)
	assert.Equal(t, []string{"continue"}, f.host.deliveries)
	f.execute(t, "stop", `{"session":`+itoa(f.work)+`}`)
	assert.Equal(t, []int64{f.work}, f.host.stopped)
	f.execute(t, "send", `{"session":`+itoa(f.work)+`,"text":"resume stopped work"}`)
	assert.Equal(t, []string{"continue", "resume stopped work"}, f.host.deliveries)
	f.execute(t, "open", `{"project":"devbox","about":"registered work","text":"start"}`)
	f.execute(t, "open", `{"project":"fresh-research","about":"scratch work","text":"research"}`)
	for _, instruction := range []string{"continue", "resume stopped work", "start", "research"} {
		var from string
		var observer int64
		require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT json_extract(attributes, '$.from'),
json_extract(attributes, '$.observer_session_id') FROM session_inbox WHERE raw_content = ?`, instruction).
			Scan(&from, &observer))
		assert.Equal(t, "assistant", from)
		assert.Equal(t, f.root, observer)
	}
	require.Len(t, f.host.created, 2)
	for index, id := range f.host.created {
		card, err := f.store.Target(t.Context(), Scope{Root: f.root, Owner: testOwner}, id)
		require.NoError(t, err)
		assert.True(t, card.Pending)
		assert.Equal(t, []string{"registered work", "scratch work"}[index], card.Title)
	}
	info, err := os.Stat(filepath.Join(f.store.projectsRoot, "fresh-research"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	for id, instance := range f.tools {
		assert.True(t, json.Valid(instance.Parameters()), id)
		assert.NotEmpty(t, instance.Description(), id)
		assert.Equal(
			t,
			id == "around" || id == "read_session" || id == "sessions" || id == "projects" || id == "focus_list" ||
				id == "models",
			instance.ParallelSafe(),
			id,
		)
	}
}

func TestAssistantToolsRefuseUnauthorizedWork(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	foreign := workSession(t, f.sessions, map[string]any{"manager_id": "other-manager"})
	child := workSession(t, f.sessions, map[string]any{"manager_id": testOwner})
	_, err := f.db.ExecContext(t.Context(), `UPDATE sessions SET parent_id = ? WHERE id = ?`, f.work, child)
	require.NoError(t, err)
	retired := workSession(t, f.sessions, map[string]any{"manager_id": testOwner})
	killed := workSession(t, f.sessions, map[string]any{"manager_id": testOwner})
	_, err = f.db.ExecContext(
		t.Context(),
		`UPDATE sessions SET killed_at = CURRENT_TIMESTAMP, status = 'killed' WHERE id IN (?, ?)`,
		killed, retired,
	)
	require.NoError(t, err)
	for _, target := range []int64{foreign, child, retired, killed, f.root} {
		for _, id := range []string{"read_session", "send", "stop"} {
			_, err := f.tools[id].Execute(
				t.Context(),
				json.RawMessage(fmt.Sprintf(`{"session":%d,"text":"go","count":1}`, target)),
			)
			require.ErrorContains(t, err, "not a work session", id)
		}
		_, err := f.tools["focus"].Execute(
			t.Context(),
			json.RawMessage(fmt.Sprintf(`{"text":"wait","sessions":[%d]}`, target)),
		)
		require.ErrorContains(t, err, "not a work session")
	}
	assert.Empty(t, f.host.deliveries)
	assert.Empty(t, f.host.stopped)
}

func TestAssistantToolValidation(t *testing.T) {
	for _, tc := range []struct{ id, args, want string }{
		{"open", `{"project":"devbox","text":"go"}`, "about must be one line"},
		{"open", `{"project":"devbox","about":"a\nb","text":"go"}`, "about must be one line"},
		{"open", `{"project":"devbox","about":"` + strings.Repeat("界", 81) + `","text":"go"}`, "about must be one line of 1-80 characters"},
		{"open", `{"project":"../outside","about":"why","text":"go"}`, "project name must not contain"},
		{"open", `{"project":"devbox","about":"why"}`, "needs a project and a text"},
		{"send", `{"session":"new-7-1","text":"go"}`, "must be a session id"},
		{"send", `{"session":2,"text":" "}`, "send needs a text"},
		{"read_session", `{"session":2,"count":61}`, "count must be between 1 and 60"},
		{"around", `{"ref":"msg 1","count":11}`, "count must be between 1 and 10"},
		{"remember", `{"text":" "}`, "needs text"},
		{"remember", `{"text":"` + strings.Repeat("ж", 301) + `"}`, "at most 300 characters"},
		{"remember", `{"text":"x","until":"next week"}`, "must be a date"},
		{"rewrite_memory", `{"id":"m999","text":"x"}`, "no memory note"},
		{"forget_memory", `{"id":"m-new-1"}`, "invalid m id"},
		{"focus", `{"text":"","sessions":[]}`, "one line of text"},
		{"focus", `{"text":"` + strings.Repeat("f", 201) + `","sessions":[]}`, "at most 200"},
		{"focus", `{"text":"x","sessions":["devbox"]}`, "must be a session id"},
		{"update_focus", `{"id":"f999","text":"x","sessions":[]}`, "no focus item"},
		{"done_focus", `{"id":"f999"}`, "no focus item"},
		{"send", `{"session":true}`, "arguments for send"},
	} {
		t.Run(tc.id+tc.want, func(t *testing.T) {
			t.Parallel()
			f := newToolFixture(t)
			_, err := f.tools[tc.id].Execute(t.Context(), json.RawMessage(tc.args))
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, f.host.created)
			assert.Empty(t, f.host.deliveries)
			notes, err := f.store.Memory(t.Context(), f.root)
			require.NoError(t, err)
			assert.Empty(t, notes)
			items, err := f.store.Focus(t.Context(), f.root)
			require.NoError(t, err)
			assert.Empty(t, items)
		})
	}
}

func newToolFixture(t *testing.T) *toolFixture {
	t.Helper()
	db, sessions, store, root := assistantFixture(t)
	f := &toolFixture{
		db:       db,
		sessions: sessions,
		store:    store,
		root:     root,
		work:     devboxWork(t, db, sessions, root),
		host:     &toolHost{sessions: sessions},
		tools:    make(map[string]tool.Tool),
	}
	for _, instance := range Tools(store, Scope{Root: root, Owner: testOwner}, f.host) {
		f.tools[instance.ID()] = instance
	}
	require.Len(t, f.tools, 18)
	return f
}

func (f *toolFixture) execute(t *testing.T, id, args string) string {
	t.Helper()
	result, err := f.tools[id].Execute(t.Context(), json.RawMessage(args))
	require.NoError(t, err)
	require.NotNil(t, result)
	return result.Output
}

func (h *toolHost) CreateWork(
	ctx context.Context,
	owner string,
	spec WorkSpec,
) (int64, string, error) {
	record, _, err := h.sessions.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: spec.ProjectID, Model: spec.Model, ReasoningLevel: spec.Effort,
		Name: spec.Name, WorkDir: spec.WorkDir,
		Attributes: map[string]any{"manager_id": owner, sessionstore.TitleAttribute: spec.Title},
	})
	if err != nil {
		return 0, "", err
	}
	h.created = append(h.created, record.ID)
	return record.ID, spec.Name, nil
}

func (h *toolHost) Deliver(
	ctx context.Context,
	id int64,
	text string,
	enqueue func(*sql.Tx, map[string]any) (*sessionstore.InboxInput, error),
) error {
	if err := h.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := enqueue(tx, h.attributes)
		return err
	}); err != nil {
		return err
	}
	h.deliveries = append(h.deliveries, text)
	return nil
}

func (h *toolHost) Stop(ctx context.Context, id int64) error {
	h.stopped = append(h.stopped, id)
	return h.sessions.UpdateSessionStatus(ctx, id, sessionstore.SessionStatusStopped)
}

func (h *toolHost) FocusChat(ctx context.Context, owner string, id int64) error {
	h.focused = append(h.focused, id)
	return h.sessions.FocusOwnerChat(ctx, owner, id)
}

func (h *toolHost) Models(context.Context) ([]Model, error) {
	h.modelsRead++
	return h.models, nil
}

func (h *toolHost) SetModel(ctx context.Context, id int64, model, effort string) error {
	h.modelCalls = append(h.modelCalls, modelArgs{Session: sessionRef(itoa(id)), Model: model, Effort: &effort})
	return h.sessions.UpdateSessionModel(ctx, id, model, effort)
}

func TestAssistantFocusLimit(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	for range focusLiveItems {
		f.execute(t, "focus", `{"text":"busy","sessions":[]}`)
	}
	_, err := f.tools["focus"].Execute(t.Context(), json.RawMessage(`{"text":"extra","sessions":[]}`))
	require.ErrorContains(t, err, "at most 30")
	items, err := f.store.Focus(t.Context(), f.root)
	require.NoError(t, err)
	assert.Len(t, items, focusLiveItems)
}

func TestAssistantMemoryLimitRefusalsWriteNothing(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	var last string
	for range 13 {
		last = f.execute(t, "remember", `{"text":"`+strings.Repeat("я", 300)+`"}`)
	}
	short := f.execute(t, "remember", `{"text":"`+strings.Repeat("я", 100)+`"}`)
	before, err := f.store.Memory(t.Context(), f.root)
	require.NoError(t, err)
	for _, tc := range []struct{ id, args string }{
		{"remember", `{"text":"x"}`},
		{"rewrite_memory", `{"id":"` + last + `","text":"` + strings.Repeat("x", 301) + `"}`},
		{"rewrite_memory", `{"id":"` + short + `","text":"` + strings.Repeat("x", 101) + `"}`},
	} {
		_, err := f.tools[tc.id].Execute(t.Context(), json.RawMessage(tc.args))
		require.Error(t, err)
		after, err := f.store.Memory(t.Context(), f.root)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	}
	f.execute(t, "rewrite_memory", `{"id":"`+last+`","text":"short"}`)
	f.execute(t, "remember", `{"text":"now fits"}`)
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	f.execute(t, "remember", `{"text":"`+strings.Repeat("x", 300)+`","until":"`+yesterday+`"}`)
}
