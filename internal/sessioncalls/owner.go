package sessioncalls

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/llmwire"
)

// PendingToolCall identifies one exact suspended invocation.
type PendingToolCall struct {
	ID   string
	Name string
}

// CallResolution describes the outcome of resolving a pending call.
type CallResolution uint8

const (
	CallResolutionInserted CallResolution = iota + 1
	CallResolutionAlreadyPresent
)

// TranscriptSession owns call settlement without executable session resources.
type TranscriptSession interface {
	PendingExternalCalls() []PendingToolCall
	ResolvePendingCall(context.Context, PendingToolCall, string) (CallResolution, error)
	SettleStoppedCalls(context.Context, string) error
	ResolveInterruptedCalls(context.Context, []PendingToolCall, string) error
	ReloadDeliveredCompletion(context.Context) error
	HasPendingWork() bool
}

// TranscriptAdapter supplies the transcript and its one-result commit operation.
type TranscriptAdapter struct {
	Messages     func(context.Context) ([]llmwire.Message, error)
	View         func() ([]llmwire.Message, error)
	InsertResult func(context.Context, PendingToolCall, string, bool) error
	Reload       func(context.Context) error
}

// Owner applies call ownership and settlement to a transcript adapter.
type Owner struct {
	adapter    TranscriptAdapter
	staged     func() map[string]string
	isExternal func(string) bool
}

var _ TranscriptSession = (*Owner)(nil)

// NewOwner creates a stable call owner over a live transcript adapter.
func NewOwner(adapter TranscriptAdapter, staged func() map[string]string, isExternal func(string) bool) *Owner {
	return &Owner{adapter: adapter, staged: staged, isExternal: isExternal}
}

// PendingExternalCalls reports unresolved calls with a registered external producer.
func (o *Owner) PendingExternalCalls() []PendingToolCall {
	snapshot, err := o.snapshotView()
	if err != nil {
		return nil
	}

	staged := o.stagedCalls()

	var pending []PendingToolCall

	for _, call := range snapshot.GlobalUnresolved {
		if staged[call.ID] != "" {
			pending = append(pending, PendingToolCall{ID: call.ID, Name: staged[call.ID]})
		}
	}

	return pending
}

// ResolvePendingCall commits one owned result, preserving replay identity.
func (o *Owner) ResolvePendingCall(ctx context.Context, call PendingToolCall, content string) (CallResolution, error) {
	if call.ID == "" || call.Name == "" {
		return 0, errors.New("resolve pending call: id and tool name are required")
	}

	snapshot, err := o.snapshot(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve pending call %q: %w", call.ID, err)
	}

	status, found := snapshot.Calls[call.ID]
	if !found {
		return 0, fmt.Errorf("resolve pending call %q: tool call not found", call.ID)
	}

	if status.Name != call.Name {
		return 0, fmt.Errorf(
			"resolve pending call %q: tool name mismatch: transcript=%q result=%q", call.ID, status.Name, call.Name,
		)
	}

	ownerName := o.stagedCalls()[call.ID]
	if ownerName == "" {
		return 0, fmt.Errorf("resolve pending call %q: no external producer owns %q", call.ID, status.Name)
	}

	if ownerName != call.Name {
		return 0, fmt.Errorf(
			"resolve pending call %q: producer name mismatch: ledger=%q result=%q", call.ID, ownerName, call.Name,
		)
	}

	if status.Resolved {
		return CallResolutionAlreadyPresent, nil
	}

	if err := o.adapter.InsertResult(ctx, call, content, false); err != nil {
		return 0, err
	}

	return CallResolutionInserted, nil
}

// SettleStoppedCalls closes external calls and the latest turn after producer fencing.
func (o *Owner) SettleStoppedCalls(ctx context.Context, content string) error {
	snapshot, err := o.snapshot(ctx)
	if err != nil {
		return err
	}

	staged := o.stagedCalls()

	for _, call := range snapshot.GlobalUnresolved {
		if staged[call.ID] == "" && snapshot.CurrentUnresolved[call.ID] != call.Name {
			continue
		}

		if err := o.adapter.InsertResult(
			ctx, PendingToolCall{ID: call.ID, Name: call.Name}, content, false,
		); err != nil {
			return fmt.Errorf("settle stopped call %q: %w", call.ID, err)
		}
	}

	return nil
}

// ResolveInterruptedCalls closes named latest-turn calls with typed failures.
func (o *Owner) ResolveInterruptedCalls(ctx context.Context, calls []PendingToolCall, content string) error {
	snapshot, err := o.snapshot(ctx)
	if err != nil {
		return err
	}

	for _, call := range calls {
		if snapshot.CurrentUnresolved[call.ID] != call.Name {
			continue
		}

		if err := o.adapter.InsertResult(ctx, call, content, true); err != nil {
			return fmt.Errorf("resolve interrupted call %q: %w", call.ID, err)
		}
	}

	return nil
}

// HasPendingWork reports unresolved latest-turn calls without external producers.
func (o *Owner) HasPendingWork() bool {
	snapshot, err := o.snapshotView()
	if err != nil {
		return false
	}

	staged := o.stagedCalls()

	for id := range snapshot.CurrentUnresolved {
		if staged[id] == "" {
			return true
		}
	}

	return false
}

// PendingExternalCallIDs classifies unresolved calls in an already-taken snapshot.
func (o *Owner) PendingExternalCallIDs(messages []llmwire.Message) map[string]bool {
	snapshot, err := Scan(messages)
	if err != nil {
		return nil
	}

	staged := o.stagedCalls()
	ids := make(map[string]bool)

	for _, call := range snapshot.GlobalUnresolved {
		if staged[call.ID] != "" || o.isExternal != nil && o.isExternal(call.Name) {
			ids[call.ID] = true
		}
	}

	return ids
}

// ReloadDeliveredCompletion replaces a live projection after a durable insert.
func (o *Owner) ReloadDeliveredCompletion(ctx context.Context) error {
	return o.adapter.Reload(ctx)
}

func (o *Owner) snapshot(ctx context.Context) (Snapshot, error) {
	messages, err := o.adapter.Messages(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	return Scan(messages)
}

func (o *Owner) snapshotView() (Snapshot, error) {
	messages, err := o.adapter.View()
	if err != nil {
		return Snapshot{}, err
	}

	return Scan(messages)
}

func (o *Owner) stagedCalls() map[string]string {
	if o.staged == nil {
		return nil
	}

	return o.staged()
}
