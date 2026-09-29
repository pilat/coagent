package session

import (
	"context"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessioncalls"
	"github.com/pilat/coagent/internal/tool"
)

// PendingToolCall identifies one exact suspended tool invocation.
type PendingToolCall = sessioncalls.PendingToolCall

// CallResolution describes the durable outcome of resolving a pending call.
type CallResolution = sessioncalls.CallResolution

const (
	CallResolutionInserted       = sessioncalls.CallResolutionInserted
	CallResolutionAlreadyPresent = sessioncalls.CallResolutionAlreadyPresent
)

func newLiveCallOwner(s *svc) *sessioncalls.Owner {
	return sessioncalls.NewOwner(
		sessioncalls.TranscriptAdapter{
			Messages: func(context.Context) ([]llmwire.Message, error) {
				return s.ms.getMessages(), nil
			},
			View: func() ([]llmwire.Message, error) { return s.ms.getMessages(), nil },
			InsertResult: func(ctx context.Context, call PendingToolCall, content string, toolError bool) error {
				if err := s.ms.addToolResultOutputTyped(
					ctx, call.ID, call.Name, content, nil, nil, toolError,
				); err != nil {
					return err
				}

				return s.ms.reloadMessages(ctx)
			},
			Reload: s.ms.reloadMessages,
		},
		func() map[string]string { return s.stagedCalls },
		tool.IsExternalCall,
	)
}

func (s *svc) HasPendingExternalCall() bool { return len(s.PendingExternalCalls()) > 0 }

func (s *svc) pendingExternalCallIDs() map[string]bool {
	return s.PendingExternalCallIDs(s.ms.getMessages())
}
