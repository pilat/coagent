package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/tool"
)

const (
	IDSubscribe   = "subscribe"
	IDUnsubscribe = "unsubscribe"

	subscribeDescription   = "Subscribe to another of the owner's root sessions: after each of its turn outcomes (an answer, an error, /stop, a budget park, /kill or /clear) you get one signal line, then none for it until you read it with read_session. A signal never carries the session's text. Refused for this session itself, subagents, killed sessions, and a session already subscribed to you."
	unsubscribeDescription = "Stop the standing signals that subscribe set up for a session. A signal still owed for a message you sent stays armed."
	watchParameters        = `{"type":"object","properties":{"session_id":{"type":"integer","description":"Root session id from list_sessions"}},"required":["session_id"]}`
)

func newWatchTools(host Host, callerID int64) []tool.Tool {
	return []tool.Tool{
		&sessionTool{
			host:              host,
			callerID:          callerID,
			id:                IDSubscribe,
			description:       subscribeDescription,
			parameters:        watchParameters,
			promptDescription: "get a signal when a session's turn ends",
			promptOrder:       5,
		},
		&sessionTool{
			host:              host,
			callerID:          callerID,
			id:                IDUnsubscribe,
			description:       unsubscribeDescription,
			parameters:        watchParameters,
			promptDescription: "stop those signals",
			promptOrder:       6,
		},
	}
}

func (t *sessionTool) subscribe(ctx context.Context, targetID int64) (*tool.Result, error) {
	id, unread, err := t.host.Subscribe(ctx, t.callerID, targetID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	output := fmt.Sprintf(subscribeAcknowledgement, id)
	if unread {
		output = fmt.Sprintf(subscribeUnreadAcknowledgement, id, id, id)
	}

	return &tool.Result{Title: fmt.Sprintf("subscribed #%d", id), Output: output}, nil
}

func (t *sessionTool) unsubscribe(ctx context.Context, targetID int64) (*tool.Result, error) {
	id, was, err := t.host.Unsubscribe(ctx, t.callerID, targetID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	output := fmt.Sprintf(notSubscribedAcknowledgement, id)
	if was {
		output = fmt.Sprintf(unsubscribeAcknowledgement, id)
	}

	return &tool.Result{Title: fmt.Sprintf("unsubscribed #%d", id), Output: output}, nil
}

func entryMarkers(entry Entry) string {
	var markers []string

	if entry.Spawned {
		markers = append(markers, "spawned by you")
	}

	if entry.Subscribed {
		markers = append(markers, "subscribed")
	}

	if entry.Armed {
		markers = append(markers, "awaiting signal")
	}

	if entry.Signalled {
		markers = append(markers, "unread signal")
	}

	return strings.Join(markers, ", ")
}
