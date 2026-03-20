package tool

import "context"

// callIDKey carries the tool_call id of the in-flight tool call through context,
// set per-goroutine in executeToolCall so concurrent calls never share an id.
type callIDKey struct{}

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
