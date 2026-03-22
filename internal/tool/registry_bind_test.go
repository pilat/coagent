package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// dispatchingTool mirrors batch: it runs its callee through the registry it is
// bound to.
type dispatchingTool struct {
	*mockTool
	reg Registry
}

var _ RegistryBound = (*dispatchingTool)(nil)

func (d *dispatchingTool) BindRegistry(reg Registry) Tool {
	return &dispatchingTool{mockTool: d.mockTool, reg: reg}
}

func (d *dispatchingTool) Execute(ctx context.Context, params json.RawMessage) (*Result, error) {
	return d.reg.Execute(ctx, string(params), json.RawMessage(`{}`))
}

func newDispatchRegistry() Registry {
	r := NewRegistry()
	r.Register(newMockTool("allowed", "Allowed"))
	r.Register(newMockTool("forbidden", "Forbidden"))
	r.Register(&dispatchingTool{mockTool: newMockTool("dispatch", "Dispatch"), reg: r})

	return r
}

// Register is the post-construction path (the daemon's gated tools), so the
// rebinding invariant has to hold there too, not only in derived views.
func TestRegistry_RegisterRebindsDispatchingTools(t *testing.T) {
	r := newDispatchRegistry()
	filtered := r.Filter([]string{"allowed"})

	filtered.Register(&dispatchingTool{mockTool: newMockTool("dispatch", "Dispatch"), reg: r})

	if _, err := filtered.Execute(context.Background(), "dispatch", []byte("forbidden")); err == nil {
		t.Error("a registered dispatcher must not reach past the tool set it is served from")
	}
	if _, err := filtered.Execute(context.Background(), "dispatch", []byte("allowed")); err != nil {
		t.Errorf("a registered dispatcher must still reach its own tool set: %v", err)
	}
}

func TestRegistry_DerivedViewsRebindDispatchingTools(t *testing.T) {
	r := newDispatchRegistry()

	filtered := r.Filter([]string{"allowed", "dispatch"})
	if _, err := filtered.Execute(context.Background(), "dispatch", []byte("forbidden")); err == nil {
		t.Error("a filtered dispatcher must not reach a tool the filter removed")
	}
	if _, err := filtered.Execute(context.Background(), "dispatch", []byte("allowed")); err != nil {
		t.Errorf("a filtered dispatcher must still reach a tool that survived: %v", err)
	}

	clone := r.Clone()
	clone.Unregister("forbidden")

	if _, err := clone.Execute(context.Background(), "dispatch", []byte("forbidden")); err == nil {
		t.Error("the clone's dispatcher must follow the clone's tool set")
	}
	if _, err := r.Execute(context.Background(), "dispatch", []byte("forbidden")); err != nil {
		t.Errorf("the original must be unaffected by the clone: %v", err)
	}
}
