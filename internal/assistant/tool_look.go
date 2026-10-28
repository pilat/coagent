package assistant

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/llmwire"
)

func (t *assistantTool) look(ctx context.Context, call llmwire.ToolCall) (string, error) {
	switch call.Name {
	case toolSearch:
		var args searchArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		hits, err := t.store.Search(ctx, t.scope, args.Query)
		if err != nil {
			return "", err
		}

		return renderHits(args.Query, hits), nil
	case toolAround:
		var args aroundArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		if args.Count < 1 || args.Count > aroundMaxCount {
			return "", fmt.Errorf("count must be between 1 and %d", aroundMaxCount)
		}

		return t.store.Around(ctx, t.scope, args.Ref, args.Count)
	case toolRead:
		return t.read(ctx, call)
	case toolSessions:
		var args sessionsArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		cards, total, err := t.store.List(
			ctx,
			t.scope,
			max(0, args.Offset),
			sessionsPage,
			strings.TrimSpace(args.Project),
		)
		if err != nil {
			return "", err
		}

		return renderSessions(cards, max(0, args.Offset), total, time.Now().UTC()), nil
	case toolProjects:
		projects, err := t.store.Projects(ctx)
		if err != nil {
			return "", err
		}

		return renderProjects(projects, time.Now().UTC()), nil
	default:
		items, err := t.store.Focus(ctx, t.scope.Root)
		if err != nil {
			return "", err
		}

		return renderFocusList(items, time.Now().UTC()), nil
	}
}

func (t *assistantTool) read(ctx context.Context, call llmwire.ToolCall) (string, error) {
	var args readArgs
	if err := decodeArgs(call, &args); err != nil {
		return "", err
	}

	id, ok := args.Session.id()
	if !ok {
		return "", fmt.Errorf("session %q must be a session id", args.Session)
	}

	if args.Count < 1 || args.Count > readMaxEntries {
		return "", fmt.Errorf("count must be between 1 and %d", readMaxEntries)
	}

	card, err := t.store.Target(ctx, t.scope, id)
	if err != nil {
		return "", err
	}

	text, err := t.store.Read(ctx, id, args.Count)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("read %s (%d):\n%s", card.Project, card.ID, text), nil
}
