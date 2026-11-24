package managercontrol

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestCreateSession_RejectsCallerSuppliedSpawnedByApp(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	for _, control := range []controllerapi.Controller{f.factory.ForManager("owner"), f.control(t, nil)} {
		for _, value := range []any{"bot", float64(12), nil} {
			_, err := control.CreateSession(t.Context(), controllerapi.SessionCreateData{
				WorkDir: f.p, Attributes: map[string]any{controllerapi.SessionAttributeSpawnedByApp: value},
			})
			require.EqualError(t, err, "spawned_by_app attribute is reserved")
		}
	}
	assert.Empty(t, f.backend.calls)
}

func TestAppRefusesHumanSettingsAndChat(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	id := f.root(t, f.p, nil)
	app := f.control(t, nil)
	calls := []struct {
		name, want string
		call       func() error
	}{
		{name: "SetSessionModel", want: "an app cannot change a session's model", call: func() error {
			return app.SetSessionModel(t.Context(), controllerapi.SessionSetModelData{SessionID: id, Model: "other"})
		}},
		{name: "SetSessionTitle", want: "an app cannot rename a session", call: func() error {
			return app.SetSessionTitle(t.Context(), "chat", id, "new title")
		}},
		{name: "SetOrchestrator", want: "an app cannot change orchestrator mode", call: func() error {
			_, err := app.SetOrchestrator(t.Context(), "chat", id, true)
			return err
		}},
		{name: "CreateProject", want: "an app cannot create projects", call: func() error {
			_, err := app.CreateProject(t.Context(), controllerapi.ProjectCreateData{Name: "new"})
			return err
		}},
		{name: "BindChat", call: func() error { return app.BindChat(t.Context(), "chat") }},
		{name: "ChatFocus", call: func() error {
			_, err := app.ChatFocus(t.Context(), "chat")
			return err
		}},
		{name: "ChatCursor", call: func() error {
			_, _, _, err := app.ChatCursor(t.Context(), "chat")
			return err
		}},
		{name: "EnterChat", call: func() error {
			_, err := app.EnterChat(t.Context(), "chat", id, 0)
			return err
		}},
		{name: "AdvanceChat", call: func() error {
			_, err := app.AdvanceChat(t.Context(), "chat", id, 1)
			return err
		}},
		{name: "SetChatFocus", call: func() error { return app.SetChatFocus(t.Context(), "chat", id) }},
		{name: "ResetChatFocus", call: func() error {
			_, err := app.ResetChatFocus(t.Context(), "chat", id)
			return err
		}},
	}
	for _, test := range calls {
		t.Run(test.name, func(t *testing.T) {
			want := test.want
			if want == "" {
				want = "an app keeps no chat; follow a root with TailOutput"
			}
			require.EqualError(t, test.call(), want)
		})
	}
	assert.Empty(t, f.backend.calls)
	record, err := f.store.GetSession(t.Context(), id)
	require.NoError(t, err)
	assert.NotContains(t, record.Attributes, controllerapi.SessionAttributeTitle)
	assert.False(t, controllerapi.OrchestratorMode(record.Attributes))
	var count int
	require.NoError(
		t,
		f.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM chat_focus WHERE manager_id = 'bot'").Scan(&count),
	)
	assert.Zero(t, count)
}

func TestOperatorStopAndKillAreHumanCommands(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	id := f.root(t, f.p, nil)
	control := f.factory.ForManager("operator")
	stopped, err := control.StopSession(t.Context(), controllerapi.SessionStopData{SessionID: id})
	require.NoError(t, err)
	assert.Equal(t, id+1, stopped)
	killed, err := control.KillSession(t.Context(), controllerapi.SessionKillData{SessionID: id})
	require.NoError(t, err)
	assert.Equal(t, id+1, killed)
	assert.Equal(t, []appBackendCall{
		{verb: "human", id: id, text: "/stop"}, {verb: "human", id: id, text: "/kill"},
	}, f.backend.calls)
}

func TestAppControllerHandlesEveryCapabilityMethod(t *testing.T) {
	appOverridden := []string{
		"CreateSession", "SendSessionMessage", "StopSession", "KillSession", "TurnOver", "ListSessions",
		"SetSessionModel", "CreateProject", "SpawnFavorites", "ListSkills", "ListRecentProjects",
		"BindChat", "ChatFocus", "ChatCursor", "ChatEntry", "EnterChat", "AdvanceChat", "SetChatFocus",
		"SetSessionTitle", "Orchestrator", "SetOrchestrator", "ResetChatFocus", "ChatOutputs", "ChatSessions",
		"SessionKilled", "Successor",
	}
	appPassThrough := []string{"Subscribe", "Unsubscribe", "ListModels"}
	var actual []string
	for _, capability := range []reflect.Type{
		reflect.TypeFor[controllerapi.Controller](), reflect.TypeFor[controllerapi.ChatFocusController](),
		reflect.TypeFor[controllerapi.LineageController](),
	} {
		for method := range capability.Methods() {
			actual = append(actual, method.Name)
		}
	}
	assert.ElementsMatch(t, append(appOverridden, appPassThrough...), actual)
	app := reflect.TypeFor[appController]()
	for field := range app.Fields() {
		assert.False(t, field.Anonymous, "app capabilities must be explicit: %s", field.Name)
	}
}
