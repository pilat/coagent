package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (t *assistantTool) search(ctx context.Context, raw json.RawMessage) (string, error) {
	var args searchArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	hits, err := t.store.Search(ctx, t.scope, args.Query)
	if err != nil {
		return "", err
	}

	return renderHits(args.Query, hits), nil
}

func (t *assistantTool) around(ctx context.Context, raw json.RawMessage) (string, error) {
	var args aroundArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	if args.Count < 1 || args.Count > aroundMaxCount {
		return "", fmt.Errorf("count must be between 1 and %d", aroundMaxCount)
	}

	return t.store.Around(ctx, t.scope, args.Ref, args.Count)
}

func (t *assistantTool) readSession(ctx context.Context, raw json.RawMessage) (string, error) {
	var args readArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	card, err := t.target(ctx, args.Session)
	if err != nil {
		return "", err
	}

	if args.Count < 1 || args.Count > readMaxEntries {
		return "", fmt.Errorf("count must be between 1 and %d", readMaxEntries)
	}

	text, err := t.store.Read(ctx, card.ID, args.Count)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("read %s (%d):\n%s", card.Project, card.ID, text), nil
}

func (t *assistantTool) sessions(ctx context.Context, raw json.RawMessage) (string, error) {
	var args sessionsArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	cards, total, err := t.store.List(ctx, t.scope, max(0, args.Offset), sessionsPage, strings.TrimSpace(args.Project))
	if err != nil {
		return "", err
	}

	models, err := t.host.Models(ctx)
	if err != nil {
		return "", fmt.Errorf("list models: %w", err)
	}

	return renderSessions(cards, max(0, args.Offset), total, time.Now().UTC(), models), nil
}

func (t *assistantTool) projects(ctx context.Context, raw json.RawMessage) (string, error) {
	projects, err := t.store.Projects(ctx)
	if err != nil {
		return "", err
	}

	return renderProjects(projects, time.Now().UTC()), nil
}

func (t *assistantTool) focusList(ctx context.Context, raw json.RawMessage) (string, error) {
	items, err := t.store.Focus(ctx, t.scope.Root)
	if err != nil {
		return "", err
	}

	return renderFocusList(items, time.Now().UTC()), nil
}

func (t *assistantTool) models(ctx context.Context, raw json.RawMessage) (string, error) {
	models, err := t.host.Models(ctx)
	if err != nil {
		return "", fmt.Errorf("list models: %w", err)
	}

	return renderModels(models), nil
}
