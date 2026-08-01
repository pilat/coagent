package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestGetSubagentResultTool_NilSpawner(t *testing.T) {
	tt := newGetSubagentResultTool(nil)

	_, err := tt.Execute(context.Background(), json.RawMessage(`{"id":1}`))
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected not-available error, got %v", err)
	}
}

func TestGetSubagentResultTool_Validation(t *testing.T) {
	tt := newGetSubagentResultTool(&mockSpawner{})

	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"id":0}`)); err == nil {
		t.Fatal("expected error for id=0")
	}

	if _, err := tt.Execute(context.Background(), json.RawMessage(`{`)); err == nil {
		t.Fatal("expected error for malformed json")
	}
}

func TestFormatChildResult(t *testing.T) {
	tests := []struct {
		name string
		res  childResult
		want []string
	}{
		{
			name: "completed with text",
			res:  childResult{ChildID: 7, Outcome: "completed", Iteration: 3, Output: "done: 42"},
			want: []string{"#7", "completed", "3 iterations", "done: 42"},
		},
		{
			name: "incomplete carries the context note",
			res: childResult{
				ChildID:   7,
				Outcome:   "incomplete",
				Iteration: 12,
				Output:    "ended without a final answer after 12 iterations",
			},
			want: []string{"incomplete", "without a final answer"},
		},
		{
			name: "legacy empty outcome falls back to a neutral label",
			res:  childResult{ChildID: 7, Outcome: "", Iteration: 2, Output: "some text"},
			want: []string{"finished", "some text"},
		},
		{
			name: "empty output falls back to a placeholder, never blank",
			res:  childResult{ChildID: 7, Outcome: "error", Iteration: 0, Output: ""},
			want: []string{"error", "(no output)"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatChildResult(tc.res)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("formatChildResult = %q, missing %q", got, w)
				}
			}
		})
	}
}

func TestGetSubagentResultTool_TerminalAndRunning(t *testing.T) {
	tests := []struct {
		name        string
		res         childResult
		wantInTitle string
		wantInOut   string
	}{
		{
			name: "terminal completed includes output",
			res: childResult{
				ChildID:   42,
				State:     "completed",
				Terminal:  true,
				Iteration: 3,
				Output:    "the answer",
			},
			wantInTitle: "completed",
			wantInOut:   "the answer",
		},
		{
			name:        "running has no result yet",
			res:         childResult{ChildID: 42, State: "running", Terminal: false, Iteration: 1},
			wantInTitle: "running",
			wantInOut:   "No result yet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sp := &mockSpawner{resultFn: func(_ context.Context, _ int64) (childResult, error) {
				return tc.res, nil
			}}
			tt := newGetSubagentResultTool(sp)

			out, err := tt.Execute(context.Background(), json.RawMessage(`{"id":42}`))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !strings.Contains(out.Title, tc.wantInTitle) {
				t.Errorf("title %q missing %q", out.Title, tc.wantInTitle)
			}

			if !strings.Contains(out.Output, tc.wantInOut) {
				t.Errorf("output %q missing %q", out.Output, tc.wantInOut)
			}

			if out.Metadata["terminal"] != tc.res.Terminal {
				t.Errorf("terminal metadata = %v, want %v", out.Metadata["terminal"], tc.res.Terminal)
			}
		})
	}
}

func TestGetSubagentResultTool_SpawnerError(t *testing.T) {
	sp := &mockSpawner{resultFn: func(_ context.Context, _ int64) (childResult, error) {
		return childResult{}, errors.New("boom")
	}}
	tt := newGetSubagentResultTool(sp)

	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"id":7}`)); err == nil {
		t.Fatal("expected spawner error to propagate")
	}
}

func TestSendToSubagentTool_NilSpawner(t *testing.T) {
	tt := newSendToSubagentTool(nil)

	_, err := tt.Execute(context.Background(), json.RawMessage(`{"id":1,"message":"go"}`))
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected not-available error, got %v", err)
	}
}

func TestSendToSubagentTool_Validation(t *testing.T) {
	tt := newSendToSubagentTool(&mockSpawner{})

	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"id":0,"message":"go"}`)); err == nil {
		t.Fatal("expected error for id=0")
	}

	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"id":1,"message":""}`)); err == nil {
		t.Fatal("expected error for empty message")
	}
}

func TestSendToSubagentTool_Delivers(t *testing.T) {
	sp := &mockSpawner{}
	tt := newSendToSubagentTool(sp)

	out, err := tt.Execute(context.Background(), json.RawMessage(`{"id":9,"message":"more work"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sp.lastSentID != 9 || sp.lastSentMsg != "more work" {
		t.Errorf("forwarded (%d,%q), want (9,\"more work\")", sp.lastSentID, sp.lastSentMsg)
	}

	if out.Metadata["id"] != int64(9) {
		t.Errorf("id metadata = %v, want 9", out.Metadata["id"])
	}
}

func TestSendToSubagentTool_SpawnerError(t *testing.T) {
	sp := &mockSpawner{sendFn: func(_ context.Context, _ int64, _ string) error {
		return errors.New("boom")
	}}
	tt := newSendToSubagentTool(sp)

	if _, err := tt.Execute(context.Background(), json.RawMessage(`{"id":3,"message":"x"}`)); err == nil {
		t.Fatal("expected spawner error to propagate")
	}
}
