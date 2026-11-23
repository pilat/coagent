package localapi

import (
	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

func outputRow(row controllerapi.ChatOutput) *coagentv1.OutputRow {
	kind := coagentv1.OutputType_OUTPUT_TYPE_UNSPECIFIED

	switch row.Type {
	case controllerapi.OutputMessage:
		kind = coagentv1.OutputType_OUTPUT_TYPE_MESSAGE
	case controllerapi.OutputSessionOpened:
		kind = coagentv1.OutputType_OUTPUT_TYPE_SESSION_OPENED
	case controllerapi.OutputSessionReplaced:
		kind = coagentv1.OutputType_OUTPUT_TYPE_SESSION_REPLACED
	case controllerapi.OutputSessionClosed:
		kind = coagentv1.OutputType_OUTPUT_TYPE_SESSION_CLOSED
	}

	return &coagentv1.OutputRow{
		Id: row.ID, SessionId: row.SessionID, SessionName: row.Name, Type: kind, Content: row.Content,
	}
}

func runtimeState(state sessionevent.State) coagentv1.RuntimeState {
	switch state {
	case sessionevent.StateRunning:
		return coagentv1.RuntimeState_RUNTIME_STATE_RUNNING
	case sessionevent.StateIdle:
		return coagentv1.RuntimeState_RUNTIME_STATE_IDLE
	case sessionevent.StateError:
		return coagentv1.RuntimeState_RUNTIME_STATE_ERROR
	default:
		return coagentv1.RuntimeState_RUNTIME_STATE_UNSPECIFIED
	}
}
