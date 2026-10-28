package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
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

var _ Host = (*toolHost)(nil)

type toolHost struct {
	sessions   *sessionstore.Store
	deliveries []string
	created    []int64
	stopped    []int64
}

type toolFixture struct {
	db         *sql.DB
	sessions   *sessionstore.Store
	store      *Store
	root, work int64
	host       *toolHost
	tools      map[string]tool.Tool
}

func TestAssistantToolsImmediateEffects(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	assert.Contains(t, f.execute(t, toolRead, `{"session":`+itoa(f.work)+`,"count":2}`), "tail ok")
	assert.Contains(t, f.execute(t, toolSessions, `{"offset":0}`), itoa(f.work))
	assert.Contains(t, f.execute(t, toolProjects, `{}`), "devbox")
	assert.Equal(t, "focus is empty", f.execute(t, toolFocusList, `{}`))
	memory := f.execute(t, toolRemember, `{"text":"searchable fact"}`)
	assert.Regexp(t, `^m[0-9]+$`, memory)
	assert.Contains(t, f.execute(t, toolSearch, `{"query":"searchable"}`), memory)
	f.execute(t, toolRewriteMemory, `{"id":"`+memory+`","text":"corrected fact"}`)
	notes, err := f.store.Memory(t.Context(), f.root)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "corrected fact", notes[0].Text)
	f.execute(t, toolForgetMemory, `{"id":"`+memory+`"}`)
	assert.Contains(t, f.execute(t, toolSearch, `{"query":"corrected"}`), "nothing matches")
	focus := f.execute(t, toolFocus, `{"text":"waiting","sessions":["`+itoa(f.work)+`",`+itoa(f.work)+`]}`)
	assert.Regexp(t, `^f[0-9]+$`, focus)
	f.execute(t, toolUpdateFocus, `{"id":"`+focus+`","text":"still waiting"}`)
	items, err := f.store.Focus(t.Context(), f.root)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, []string{itoa(f.work)}, items[0].Sessions, "omitting sessions preserves the wait")
	assert.Contains(t, f.execute(t, toolFocusList, `{}`), "still waiting → "+itoa(f.work))
	f.execute(t, toolUpdateFocus, `{"id":"`+focus+`","text":"finished","sessions":[]}`)
	items, err = f.store.Focus(t.Context(), f.root)
	require.NoError(t, err)
	assert.Empty(t, items[0].Sessions)
	f.execute(t, toolDoneFocus, `{"id":"`+focus+`"}`)
	assert.Equal(t, "focus is empty", f.execute(t, toolFocusList, `{}`))
	_, final := answered(t, f.sessions, f.work, "other request", "answer with context")
	assert.Contains(t, f.execute(t, toolAround, fmt.Sprintf(`{"ref":"msg %d","count":2}`, final)), "other request")
	f.execute(t, toolSend, `{"session":"`+itoa(f.work)+`","text":"continue"}`)
	assert.Equal(t, []string{"continue"}, f.host.deliveries)
	f.execute(t, toolStop, `{"session":`+itoa(f.work)+`}`)
	assert.Equal(t, []int64{f.work}, f.host.stopped)
	f.execute(t, toolSend, `{"session":`+itoa(f.work)+`,"text":"resume stopped work"}`)
	assert.Equal(t, []string{"continue", "resume stopped work"}, f.host.deliveries)
	f.execute(t, toolOpen, `{"project":"devbox","about":"registered work","text":"start"}`)
	f.execute(t, toolOpen, `{"project":"fresh-research","about":"scratch work","text":"research"}`)
	require.Len(t, f.host.created, 2)
	for _, id := range f.host.created {
		card, err := f.store.Target(t.Context(), Scope{Root: f.root, Owner: testOwner}, id)
		require.NoError(t, err)
		assert.True(t, card.Pending)
		assert.NotEmpty(t, card.About)
	}
	info, err := os.Stat(filepath.Join(f.store.projectsRoot, "fresh-research"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	for id, instance := range f.tools {
		assert.True(t, json.Valid(instance.Parameters()), id)
		assert.NotEmpty(t, instance.Description(), id)
		assert.Equal(
			t,
			id == toolAround || id == toolRead || id == toolSessions || id == toolProjects || id == toolFocusList,
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
	management := workSession(t, f.sessions, map[string]any{"manager_id": testOwner, "management_surface": true})
	killed := workSession(t, f.sessions, map[string]any{"manager_id": testOwner})
	_, err = f.db.ExecContext(
		t.Context(),
		`UPDATE sessions SET killed_at = CURRENT_TIMESTAMP, status = 'killed' WHERE id = ?`,
		killed,
	)
	require.NoError(t, err)
	for _, target := range []int64{foreign, child, management, killed, f.root} {
		for _, id := range []string{toolRead, toolSend, toolStop} {
			_, err := f.tools[id].Execute(
				t.Context(),
				json.RawMessage(fmt.Sprintf(`{"session":%d,"text":"go","count":1}`, target)),
			)
			require.ErrorContains(t, err, "not a work session", id)
		}
		_, err := f.tools[toolFocus].Execute(
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
		{toolOpen, `{"project":"devbox","text":"go"}`, "about must be one line"},
		{toolOpen, `{"project":"devbox","about":"a\nb","text":"go"}`, "about must be one line"},
		{toolOpen, `{"project":"../outside","about":"why","text":"go"}`, "project name must not contain"},
		{toolOpen, `{"project":"devbox","about":"why"}`, "needs a project and a text"},
		{toolSend, `{"session":"new-7-1","text":"go"}`, "must be a session id"},
		{toolSend, `{"session":2,"text":" "}`, "send needs a text"},
		{toolRead, `{"session":2,"count":61}`, "count must be between 1 and 60"},
		{toolAround, `{"ref":"msg 1","count":11}`, "count must be between 1 and 10"},
		{toolRemember, `{"text":" "}`, "needs text"},
		{toolRemember, `{"text":"` + strings.Repeat("ж", 301) + `"}`, "at most 300 characters"},
		{toolRemember, `{"text":"x","until":"next week"}`, "must be a date"},
		{toolRewriteMemory, `{"id":"m999","text":"x"}`, "no memory note"},
		{toolForgetMemory, `{"id":"m-new-1"}`, "invalid m id"},
		{toolFocus, `{"text":"","sessions":[]}`, "one line of text"},
		{toolFocus, `{"text":"` + strings.Repeat("f", 201) + `","sessions":[]}`, "at most 200"},
		{toolFocus, `{"text":"x","sessions":["devbox"]}`, "must be a session id"},
		{toolUpdateFocus, `{"id":"f999","text":"x","sessions":[]}`, "no focus item"},
		{toolDoneFocus, `{"id":"f999"}`, "no focus item"},
		{toolSend, `{"session":true}`, "arguments for send"},
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
	require.Len(t, f.tools, 15)
	return f
}

func (f *toolFixture) execute(t *testing.T, id, args string) string {
	t.Helper()
	result, err := f.tools[id].Execute(t.Context(), json.RawMessage(args))
	require.NoError(t, err)
	require.NotNil(t, result)
	return result.Output
}

func (h *toolHost) CreateWork(ctx context.Context, owner string, projectID int64, name, workDir string) (int64, error) {
	record, err := h.sessions.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": owner})
	if err != nil {
		return 0, err
	}
	h.created = append(h.created, record.ID)
	return record.ID, nil
}

func (h *toolHost) Deliver(ctx context.Context, id int64, text string, enqueue func(*sql.Tx) error) error {
	if err := h.sessions.WithTx(ctx, enqueue); err != nil {
		return err
	}
	h.deliveries = append(h.deliveries, text)
	return nil
}

func (h *toolHost) Stop(ctx context.Context, id int64) error {
	h.stopped = append(h.stopped, id)
	return h.sessions.UpdateSessionStatus(ctx, id, sessionstore.SessionStatusStopped)
}

func TestAssistantFocusLimit(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	for range focusLiveItems {
		f.execute(t, toolFocus, `{"text":"busy","sessions":[]}`)
	}
	_, err := f.tools[toolFocus].Execute(t.Context(), json.RawMessage(`{"text":"extra","sessions":[]}`))
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
		last = f.execute(t, toolRemember, `{"text":"`+strings.Repeat("я", 300)+`"}`)
	}
	short := f.execute(t, toolRemember, `{"text":"`+strings.Repeat("я", 100)+`"}`)
	before, err := f.store.Memory(t.Context(), f.root)
	require.NoError(t, err)
	for _, tc := range []struct{ id, args string }{
		{toolRemember, `{"text":"x"}`},
		{toolRewriteMemory, `{"id":"` + last + `","text":"` + strings.Repeat("x", 301) + `"}`},
		{toolRewriteMemory, `{"id":"` + short + `","text":"` + strings.Repeat("x", 101) + `"}`},
	} {
		_, err := f.tools[tc.id].Execute(t.Context(), json.RawMessage(tc.args))
		require.Error(t, err)
		after, err := f.store.Memory(t.Context(), f.root)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	}
	f.execute(t, toolRewriteMemory, `{"id":"`+last+`","text":"short"}`)
	f.execute(t, toolRemember, `{"text":"now fits"}`)
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	f.execute(t, toolRemember, `{"text":"`+strings.Repeat("x", 300)+`","until":"`+yesterday+`"}`)
}
