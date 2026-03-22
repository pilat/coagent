package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// mockTool is a simple tool implementation for testing.
type mockTool struct {
	id          string
	description string
	params      json.RawMessage
	execFunc    func(ctx context.Context, params json.RawMessage) (*Result, error)
}

func (m *mockTool) ID() string                  { return m.id }
func (m *mockTool) Description() string         { return m.description }
func (m *mockTool) Parameters() json.RawMessage { return m.params }
func (m *mockTool) Execute(ctx context.Context, params json.RawMessage) (*Result, error) {
	if m.execFunc != nil {
		return m.execFunc(ctx, params)
	}
	return &Result{Output: "mock output"}, nil
}

func newMockTool(id, desc string) *mockTool {
	return &mockTool{
		id:          id,
		description: desc,
		params:      json.RawMessage(`{"type":"object"}`),
	}
}

func TestNewRegistry(t *testing.T) {
	r := NewRegistry()
	if r == nil {
		t.Fatal("NewRegistry returned nil")
	}
	if len(r.List()) != 0 {
		t.Errorf("NewRegistry should return empty registry, got %d tools", len(r.List()))
	}
}

func TestRegistry_Register(t *testing.T) {
	r := NewRegistry()
	tool := newMockTool("test", "A test tool")

	r.Register(tool)

	if got := r.Get("test"); got == nil {
		t.Error("Get returned nil after Register")
	} else if got.ID() != "test" {
		t.Errorf("Get returned wrong tool, got ID %s", got.ID())
	}
}

func TestRegistry_Register_Overwrite(t *testing.T) {
	r := NewRegistry()
	tool1 := newMockTool("test", "First version")
	tool2 := newMockTool("test", "Second version")

	r.Register(tool1)
	r.Register(tool2)

	got := r.Get("test")
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Description() != "Second version" {
		t.Errorf("Register did not overwrite, got description %s", got.Description())
	}
}

func TestRegistry_Get_NotFound(t *testing.T) {
	r := NewRegistry()

	if got := r.Get("nonexistent"); got != nil {
		t.Errorf("Get should return nil for nonexistent tool, got %v", got)
	}
}

func TestRegistry_List(t *testing.T) {
	r := NewRegistry()
	r.Register(newMockTool("tool1", "Tool 1"))
	r.Register(newMockTool("tool2", "Tool 2"))
	r.Register(newMockTool("tool3", "Tool 3"))

	list := r.List()
	if len(list) != 3 {
		t.Errorf("List returned %d tools, expected 3", len(list))
	}

	// Check all tools are present
	ids := make(map[string]bool)
	for _, tool := range list {
		ids[tool.ID()] = true
	}
	for _, id := range []string{"tool1", "tool2", "tool3"} {
		if !ids[id] {
			t.Errorf("List missing tool %s", id)
		}
	}
}

func TestRegistry_IDs(t *testing.T) {
	r := NewRegistry()
	r.Register(newMockTool("alpha", "Alpha"))
	r.Register(newMockTool("beta", "Beta"))

	ids := r.IDs()
	if len(ids) != 2 {
		t.Errorf("IDs returned %d items, expected 2", len(ids))
	}

	idMap := make(map[string]bool)
	for _, id := range ids {
		idMap[id] = true
	}
	if !idMap["alpha"] || !idMap["beta"] {
		t.Errorf("IDs missing expected values, got %v", ids)
	}
}

func TestRegistry_Execute(t *testing.T) {
	r := NewRegistry()
	expectedOutput := "execution result"
	tool := &mockTool{
		id:          "exec",
		description: "Executable tool",
		params:      json.RawMessage(`{}`),
		execFunc: func(ctx context.Context, params json.RawMessage) (*Result, error) {
			return &Result{
				Title:  "Executed",
				Output: expectedOutput,
				Metadata: map[string]any{
					"key": "value",
				},
			}, nil
		},
	}
	r.Register(tool)

	result, err := r.Execute(context.Background(), "exec", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Output != expectedOutput {
		t.Errorf("Execute returned wrong output: got %s, want %s", result.Output, expectedOutput)
	}
	if result.Title != "Executed" {
		t.Errorf("Execute returned wrong title: got %s", result.Title)
	}
	if result.Metadata["key"] != "value" {
		t.Errorf("Execute returned wrong metadata: %v", result.Metadata)
	}
}

func TestRegistry_Execute_NotFound(t *testing.T) {
	r := NewRegistry()

	_, err := r.Execute(context.Background(), "nonexistent", json.RawMessage(`{}`))
	if err == nil {
		t.Error("Execute should return error for nonexistent tool")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	r.Register(newMockTool("remove-me", "To be removed"))

	if !r.Unregister("remove-me") {
		t.Error("Unregister returned false for existing tool")
	}
	if r.Get("remove-me") != nil {
		t.Error("Tool still exists after Unregister")
	}
}

func TestRegistry_Unregister_NotFound(t *testing.T) {
	r := NewRegistry()

	if r.Unregister("nonexistent") {
		t.Error("Unregister returned true for nonexistent tool")
	}
}

func TestRegistry_Clone(t *testing.T) {
	r := NewRegistry()
	r.Register(newMockTool("tool1", "Tool 1"))
	r.Register(newMockTool("tool2", "Tool 2"))

	clone := r.Clone()

	// Clone should have the same tools
	if len(clone.List()) != 2 {
		t.Errorf("Clone has %d tools, expected 2", len(clone.List()))
	}

	// Modifying clone should not affect original
	clone.Register(newMockTool("tool3", "Tool 3"))
	if len(r.List()) != 2 {
		t.Error("Modifying clone affected original registry")
	}
}

func TestRegistry_Filter(t *testing.T) {
	r := NewRegistry()
	r.Register(newMockTool("keep1", "Keep 1"))
	r.Register(newMockTool("keep2", "Keep 2"))
	r.Register(newMockTool("remove", "Remove"))

	filtered := r.Filter([]string{"keep1", "keep2", "nonexistent"})

	if len(filtered.List()) != 2 {
		t.Errorf("Filter returned %d tools, expected 2", len(filtered.List()))
	}
	if filtered.Get("keep1") == nil {
		t.Error("Filter missing keep1")
	}
	if filtered.Get("keep2") == nil {
		t.Error("Filter missing keep2")
	}
	if filtered.Get("remove") != nil {
		t.Error("Filter should not contain remove")
	}
}

func TestRegistry_Concurrent(t *testing.T) {
	r := NewRegistry()
	done := make(chan bool)

	for i := range 10 {
		go func(n int) {
			r.Register(newMockTool(string(rune('A'+n)), "Tool"))
			done <- true
		}(i)
	}

	for range 10 {
		go func() {
			_ = r.List()
			_ = r.IDs()
			done <- true
		}()
	}

	for range 20 {
		<-done
	}
}
