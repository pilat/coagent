package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/pilat/coagent/internal/controllerapi"
)

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakeController struct {
	mu            sync.Mutex
	focus         int64
	boundChats    map[string]bool
	outputs       map[int64][]controllerapi.ChatOutput
	cursor        int64
	entered       bool
	managerID     string
	listDirResult *controllerapi.FsListDirResultData

	listSessions []controllerapi.SessionInfo
	listSkills   []controllerapi.ConfigSkillInfo
	messageCalls []controllerapi.SessionMessageData
	titleCalls   []struct {
		id    int64
		title string
	}

	createSessionCalls  []controllerapi.SessionCreateData
	createSessionErr    error
	createProjectCalls  []controllerapi.ProjectCreateData
	createProjectResult *controllerapi.ProjectCreateResultData
	createProjectErr    error
	recentProjects      []controllerapi.RecentProjectInfo
	recentProjectsErr   error

	listModels      []controllerapi.ConfigModelInfo
	listModelsCalls int
	setModelCalls   []controllerapi.SessionSetModelData
	setModelErr     error
}

func (f *fakeController) CreateSession(_ context.Context, data controllerapi.SessionCreateData) (int64, error) {
	f.createSessionCalls = append(f.createSessionCalls, data)
	if f.createSessionErr != nil {
		return 0, f.createSessionErr
	}

	id := int64(len(f.createSessionCalls))
	f.listSessions = append(f.listSessions, controllerapi.SessionInfo{
		ID: id, Name: fmt.Sprintf("project - %d", id), WorkDir: data.WorkDir,
		Attributes: map[string]any{"manager_id": f.owner()},
	})
	return id, nil
}

func (f *fakeController) CreateProject(
	_ context.Context,
	data controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
	f.createProjectCalls = append(f.createProjectCalls, data)

	if f.createProjectErr != nil {
		return nil, f.createProjectErr
	}

	if f.createProjectResult != nil {
		return f.createProjectResult, nil
	}

	return &controllerapi.ProjectCreateResultData{ID: 1, Name: data.Name, Path: "/root/" + data.Name}, nil
}

func (f *fakeController) ListRecentProjects(context.Context) (*controllerapi.ProjectListResultData, error) {
	if f.recentProjectsErr != nil {
		return nil, f.recentProjectsErr
	}

	return &controllerapi.ProjectListResultData{Projects: f.recentProjects}, nil
}

func (f *fakeController) ListSessions(context.Context) ([]controllerapi.SessionInfo, error) {
	return f.listSessions, nil
}

func (f *fakeController) SendSessionMessage(_ context.Context, data controllerapi.SessionMessageData) error {
	f.messageCalls = append(f.messageCalls, data)
	return nil
}

func (f *fakeController) SetSessionModel(_ context.Context, data controllerapi.SessionSetModelData) error {
	f.setModelCalls = append(f.setModelCalls, data)
	return f.setModelErr
}

func (f *fakeController) ListDir(
	context.Context,
	controllerapi.FsListDirData,
) (*controllerapi.FsListDirResultData, error) {
	return f.listDirResult, nil
}

func (f *fakeController) ListModels(context.Context) (*controllerapi.ConfigModelsResultData, error) {
	f.listModelsCalls++
	return &controllerapi.ConfigModelsResultData{Models: f.listModels}, nil
}

func (f *fakeController) ListSkills(
	context.Context,
	controllerapi.ConfigSkillsData,
) (*controllerapi.ConfigSkillsResultData, error) {
	return &controllerapi.ConfigSkillsResultData{Skills: f.listSkills}, nil
}

func (f *fakeController) Subscribe() <-chan controllerapi.SessionNotification {
	ch := make(chan controllerapi.SessionNotification)
	close(ch)
	return ch
}

func (f *fakeController) Unsubscribe(ch <-chan controllerapi.SessionNotification) {}

func telegramResponse(req *http.Request, payload string) *http.Response {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(payload)),
		Header:     make(http.Header),
		Request:    req,
	}

	return resp
}

func (f *fakeController) owner() string {
	if f.managerID == "" {
		return "telegram-main"
	}
	return f.managerID
}

func (f *fakeController) ChatFocus(context.Context, string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.focus, nil
}

func (f *fakeController) BindChat(_ context.Context, chatID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.boundChats == nil {
		f.boundChats = make(map[string]bool)
	}
	f.boundChats[chatID] = true
	return nil
}

func (f *fakeController) SetChatFocus(_ context.Context, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, session := range f.listSessions {
		if session.ID == id && session.KilledAt == nil && session.Status != "terminating" &&
			session.Status != "killed" && session.Attributes["manager_id"] == f.owner() {
			f.focus, f.cursor, f.entered = id, 0, false
			return nil
		}
	}
	return fmt.Errorf("#%d is not a live session", id)
}

func (f *fakeController) ResetChatFocus(_ context.Context, _ string, from int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.focus != from {
		return false, nil
	}
	f.focus, f.cursor, f.entered = 0, 0, false
	return true, nil
}

func (f *fakeController) ChatOutputs(
	_ context.Context,
	_ string,
	id, after int64,
	limit int,
) ([]controllerapi.ChatOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []controllerapi.ChatOutput
	for _, row := range f.outputs[id] {
		if row.ID > after {
			row.Name = f.chatName(id)
			rows = append(rows, row)
			if len(rows) == limit {
				break
			}
		}
	}
	return rows, nil
}

func (f *fakeController) ChatCursor(context.Context, string) (int64, int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.focus, f.cursor, f.entered, nil
}

func (f *fakeController) ChatEntry(
	_ context.Context,
	_ string,
	id int64,
	limit int,
) ([]controllerapi.ChatOutput, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []controllerapi.ChatOutput
	var newest int64
	for _, row := range f.outputs[id] {
		newest = max(newest, row.ID)
		if row.Type != controllerapi.OutputSessionOpened && row.Type != controllerapi.OutputSessionReplaced {
			row.Name = f.chatName(id)
			rows = append(rows, row)
		}
	}
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	return rows, newest, nil
}

func (f *fakeController) EnterChat(_ context.Context, _ string, id, newest int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.focus != id || f.entered {
		return false, nil
	}
	f.cursor, f.entered = newest, true
	return true, nil
}

func (f *fakeController) AdvanceChat(_ context.Context, _ string, id, outputID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.focus != id || !f.entered || f.cursor >= outputID {
		return false, nil
	}
	f.cursor = outputID
	return true, nil
}

func (f *fakeController) ChatSessions(context.Context, string) ([]controllerapi.ChatSessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []controllerapi.ChatSessionInfo
	for _, session := range f.listSessions {
		if !isActiveSession(session) || session.Attributes["manager_id"] != f.owner() {
			continue
		}
		rows = append(rows, controllerapi.ChatSessionInfo{ID: session.ID, Name: session.Name})
	}
	return rows, nil
}

func (f *fakeController) SessionKilled(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, session := range f.listSessions {
		if session.ID == id {
			return session.KilledAt != nil, nil
		}
	}
	return false, nil
}

func (f *fakeController) SetSessionTitle(_ context.Context, _ string, id int64, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titleCalls = append(f.titleCalls, struct {
		id    int64
		title string
	}{id, title})
	return nil
}

func harnessResponse(req *http.Request, payload string) *http.Response {
	return telegramResponse(req, payload)
}

func (f *fakeController) chatName(id int64) string {
	for _, session := range f.listSessions {
		if session.ID == id {
			return session.Name
		}
	}
	return ""
}
