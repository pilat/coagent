package tool

import "context"

// callIDKey carries the tool_call id of the in-flight tool call through context,
// set per-goroutine in executeToolCall so concurrent calls never share an id.
type callIDKey struct{}

type activationKey struct{}

// ActivationGrant identifies the one durable user-command authority available
// to the current assistant turn.
type ActivationGrant struct {
	SessionID  int64
	InputID    int64
	ToolID     string
	Command    string
	ToolCallID string
}

// WithCallID returns a context carrying the given tool_call id.
func WithCallID(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, callIDKey{}, callID)
}

// CallIDFromContext returns the tool_call id set by WithCallID, or "" if absent.
func CallIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(callIDKey{}).(string); ok {
		return v
	}

	return ""
}

func WithActivationGrant(ctx context.Context, grant ActivationGrant) context.Context {
	return context.WithValue(ctx, activationKey{}, grant)
}

func ActivationGrantFromContext(ctx context.Context) (ActivationGrant, bool) {
	grant, ok := ctx.Value(activationKey{}).(ActivationGrant)

	return grant, ok
}
