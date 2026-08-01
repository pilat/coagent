package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
)

// mockSpawner implements the daemon spawner interface for task-tool tests.
type mockSpawner struct {
	lastReq       spawnRequest
	spawnCount    int
	spawnFn       func(ctx context.Context, req spawnRequest) (childResult, error)
	linkPendingFn func(ctx context.Context, parentID int64, taskCallID string) (bool, error)
	resultFn      func(ctx context.Context, childID int64) (childResult, error)
	sendFn        func(ctx context.Context, childID int64, msg string) error
	lastSentID    int64
	lastSentMsg   string
}

func (m *mockSpawner) Spawn(ctx context.Context, req spawnRequest) (childResult, error) {
	m.lastReq = req
	m.spawnCount++

	if m.spawnFn != nil {
		return m.spawnFn(ctx, req)
	}

	return childResult{ChildID: 123, State: "spawned"}, nil
}

func (m *mockSpawner) Result(ctx context.Context, childID int64) (childResult, error) {
	if m.resultFn != nil {
		return m.resultFn(ctx, childID)
	}

	return childResult{ChildID: childID, State: "completed", Terminal: true}, nil
}

func (m *mockSpawner) SendToChild(ctx context.Context, childID int64, msg string) error {
	m.lastSentID = childID
	m.lastSentMsg = msg

	if m.sendFn != nil {
		return m.sendFn(ctx, childID, msg)
	}

	return nil
}

func (m *mockSpawner) LinkPending(ctx context.Context, parentID int64, taskCallID string) (bool, error) {
	if m.linkPendingFn != nil {
		return m.linkPendingFn(ctx, parentID, taskCallID)
	}

	return false, nil
}

func taskToolWith(sp *mockSpawner) tool.Tool {
	return newTaskTool(sp, 7, registry.NewSet(nil), nil)
}

func TestTaskTool_BackgroundSpawns(t *testing.T) {
	sp := &mockSpawner{}
	tt := taskToolWith(sp)

	params, _ := json.Marshal(TaskParams{
		Prompt:       "do something",
		Description:  "bg task",
		SubagentType: "general",
		Background:   true,
	})

	ctx := tool.WithCallID(context.Background(), "call-bg")

	result, err := tt.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if sp.lastReq.Blocking {
		t.Error("background task must spawn with Blocking=false")
	}

	if sp.lastReq.TaskCallID != "call-bg" {
		t.Errorf("expected TaskCallID call-bg, got %q", sp.lastReq.TaskCallID)
	}

	if sp.lastReq.ParentID != 7 {
		t.Errorf("parent id not propagated: %+v", sp.lastReq)
	}

	if !strings.Contains(result.Output, "id: 123") {
		t.Errorf("output missing child id metadata: %s", result.Output)
	}

	if !strings.Contains(result.Output, "delivered automatically and wake this session") {
		t.Errorf("output must explain automatic wake-up: %s", result.Output)
	}

	if strings.Contains(result.Output, "Poll its progress") {
		t.Errorf("output must not encourage polling: %s", result.Output)
	}
}

func TestSubagentToolDescriptionsTeachExecutionContract(t *testing.T) {
	task := taskToolWith(&mockSpawner{})
	taskDescription := task.Description()
	taskParameters := string(task.Parameters())
	if strings.Contains(taskParameters, `"id"`) {
		t.Errorf("task must not advertise the removed fake resume path: %s", taskParameters)
	}

	for _, want := range []string{
		"Foreground (background omitted or false): use when you need the answer before continuing",
		"Background (background=true): use only when you can continue useful independent work",
		"completion is delivered automatically as a subagent_event and wakes you",
		"Never use sleep, schedule, or repeated get_subagent_result calls to wait for subagents",
	} {
		if !strings.Contains(taskDescription, want) {
			t.Errorf("task description missing %q:\n%s", want, taskDescription)
		}
	}

	if strings.Contains(taskParameters, "poll with get_subagent_result") {
		t.Errorf("background parameter must not encourage polling: %s", taskParameters)
	}

	for _, want := range []string{
		"Set true only when you can continue useful independent work without the answer",
		"completion is delivered automatically and wakes the parent",
		"Never use sleep or get_subagent_result polling to wait for it",
	} {
		if !strings.Contains(taskParameters, want) {
			t.Errorf("background parameter missing %q: %s", want, taskParameters)
		}
	}

	resultTool := newGetSubagentResultTool(&mockSpawner{})
	resultDescription := resultTool.Description()
	for _, want := range []string{
		"one-off diagnostic snapshot",
		"not waiting: do not poll this tool",
		"Completion is delivered automatically as a subagent_event and wakes the parent session",
	} {
		if !strings.Contains(resultDescription, want) {
			t.Errorf("get_subagent_result description missing %q:\n%s", want, resultDescription)
		}
	}

	for _, forbidden := range []string{"check again later", "polling is optional"} {
		if strings.Contains(resultDescription, forbidden) {
			t.Errorf("get_subagent_result description encourages polling with %q: %s", forbidden, resultDescription)
		}
	}

	sendTool := newSendToSubagentTool(&mockSpawner{})
	sendDescription := sendTool.Description()
	for _, want := range []string{
		"same subagent session previously launched with task, whether it was foreground or background",
		"preserving that session's full context",
		"not a status check or a way to wait",
		"completion is delivered automatically and wakes the parent session",
		"Do not use sleep, schedule, or polling",
	} {
		if !strings.Contains(sendDescription, want) {
			t.Errorf("send_to_subagent description missing %q:\n%s", want, sendDescription)
		}
	}
}

func TestGetSubagentResult_RunningOutputIsDiagnosticNotPollingPrompt(t *testing.T) {
	sp := &mockSpawner{
		resultFn: func(_ context.Context, childID int64) (childResult, error) {
			return childResult{ChildID: childID, State: "running", Iteration: 2}, nil
		},
	}
	result, err := newGetSubagentResultTool(sp).Execute(context.Background(), []byte(`{"id":123}`))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if !strings.Contains(result.Output, "diagnostic snapshot") ||
		!strings.Contains(result.Output, "delivered automatically and wake this session") {
		t.Errorf("running output must explain snapshot and automatic wake-up: %s", result.Output)
	}
	if strings.Contains(result.Output, "check again") {
		t.Errorf("running output must not encourage polling: %s", result.Output)
	}
}

func TestSendToSubagent_OutputConfirmsDurableAcceptance(t *testing.T) {
	result, err := newSendToSubagentTool(&mockSpawner{}).Execute(
		context.Background(),
		[]byte(`{"id":123,"message":"do more"}`),
	)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if !strings.Contains(result.Output, "Follow-up durably accepted for subagent session #123") ||
		!strings.Contains(result.Output, "delivered automatically and wake this session") {
		t.Errorf("send output must confirm durable acceptance and automatic wake-up: %s", result.Output)
	}
}

func TestTaskTool_BlockingSuspends(t *testing.T) {
	sp := &mockSpawner{}
	tt := taskToolWith(sp)

	params, _ := json.Marshal(TaskParams{
		Prompt:       "do something",
		Description:  "blocking task",
		SubagentType: "general",
	})

	ctx := tool.WithCallID(context.Background(), "call-block")

	_, err := tt.Execute(ctx, params)
	if !errors.Is(err, tool.ErrSuspend) {
		t.Fatalf("blocking task must suspend, got err=%v", err)
	}

	if !sp.lastReq.Blocking {
		t.Error("blocking task must spawn with Blocking=true")
	}

	if sp.spawnCount != 1 {
		t.Errorf("expected exactly one spawn, got %d", sp.spawnCount)
	}
}

func TestTaskTool_BlockingResumeIsIdempotent(t *testing.T) {
	sp := &mockSpawner{
		linkPendingFn: func(_ context.Context, parentID int64, taskCallID string) (bool, error) {
			if parentID != 7 || taskCallID != "call-resume" {
				t.Errorf("unexpected LinkPending args: parent=%d call=%q", parentID, taskCallID)
			}

			return true, nil // child still in flight
		},
	}
	tt := taskToolWith(sp)

	params, _ := json.Marshal(TaskParams{
		Prompt:       "do something",
		Description:  "blocking task",
		SubagentType: "general",
	})

	ctx := tool.WithCallID(context.Background(), "call-resume")

	_, err := tt.Execute(ctx, params)
	if !errors.Is(err, tool.ErrSuspend) {
		t.Fatalf("resume of live link must re-suspend, got err=%v", err)
	}

	if sp.spawnCount != 0 {
		t.Errorf("resume of a live link must NOT re-fork, spawnCount=%d", sp.spawnCount)
	}
}

func TestTaskTool_ResolvesAgentTypeModel(t *testing.T) {
	sp := &mockSpawner{}
	set := registry.NewSet([]registry.AgentTypeConfig{{
		Name:  "reviewer",
		Mode:  registry.ModeSubagent,
		Model: "agent-model",
	}})
	tt := newTaskTool(sp, 7, set, nil)

	params, _ := json.Marshal(TaskParams{
		Prompt:       "review it",
		Description:  "bg task",
		SubagentType: "reviewer",
		Background:   true,
	})

	if _, err := tt.Execute(tool.WithCallID(context.Background(), "call-model"), params); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if sp.lastReq.AgentModel != "agent-model" {
		t.Errorf("AgentModel = %q, want agent-model", sp.lastReq.AgentModel)
	}
}

func TestTaskTool_RequiresCallIdentity(t *testing.T) {
	for _, background := range []bool{false, true} {
		sp := &mockSpawner{}
		tt := taskToolWith(sp)
		params, _ := json.Marshal(TaskParams{
			Prompt:       "do something",
			Description:  "task",
			SubagentType: "general",
			Background:   background,
		})

		_, err := tt.Execute(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "tool call id") {
			t.Fatalf("background=%t: expected tool call identity error, got %v", background, err)
		}
		if sp.spawnCount != 0 {
			t.Fatalf("background=%t: unidentifiable task must not spawn", background)
		}
	}
}

func TestTaskTool_RejectsUnknownSubagentType(t *testing.T) {
	tt := taskToolWith(&mockSpawner{})

	params, _ := json.Marshal(TaskParams{
		Prompt:       "do it",
		Description:  "task",
		SubagentType: "does-not-exist",
	})

	if _, err := tt.Execute(context.Background(), params); err == nil {
		t.Fatal("expected error for unknown subagent type")
	}
}

func TestTaskTool_NoSpawnerErrors(t *testing.T) {
	tt := newTaskTool(nil, 0, registry.NewSet(nil), nil)

	params, _ := json.Marshal(TaskParams{
		Prompt:       "do something",
		Description:  "task",
		SubagentType: "general",
	})

	if _, err := tt.Execute(context.Background(), params); err == nil {
		t.Fatal("expected error when no spawner is wired")
	}
}

func TestTaskTool_MetadataFormat(t *testing.T) {
	id := int64(789)
	got := taskMetadata(id)
	expected := "\n\n<task_metadata>\nid: 789\n</task_metadata>"

	if got != expected {
		t.Errorf("taskMetadata() = %q, want %q", got, expected)
	}
}
