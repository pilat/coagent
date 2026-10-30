package assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (t *assistantTool) work(ctx context.Context, call llmwire.ToolCall) (string, error) {
	if call.Name == toolOpen {
		return t.open(ctx, call)
	}

	var args sendArgs
	if err := decodeArgs(call, &args); err != nil {
		return "", err
	}

	id, ok := args.Session.id()
	if !ok {
		return "", fmt.Errorf("session %q must be a session id", args.Session)
	}

	if call.Name == toolStop {
		if err := t.store.Stop(ctx, t.host, t.scope, id); err != nil {
			return "", err
		}

		return fmt.Sprintf("stopped session %d", id), nil
	}

	if strings.TrimSpace(args.Text) == "" {
		return "", errors.New("send needs a text")
	}

	if err := t.store.Send(ctx, t.host, t.scope, id, strings.TrimSpace(args.Text)); err != nil {
		return "", err
	}

	return fmt.Sprintf("sent to session %d", id), nil
}

func (t *assistantTool) open(ctx context.Context, call llmwire.ToolCall) (string, error) {
	var args openArgs
	if err := decodeArgs(call, &args); err != nil {
		return "", err
	}

	args.Project = strings.TrimSpace(args.Project)
	args.Text = strings.TrimSpace(args.Text)

	if args.Project == "" || args.Text == "" {
		return "", errors.New("open needs a project and a text")
	}

	title, ok := sessionstore.NormalizeTitle(args.About)
	if !ok {
		return "", errors.New("about must be one line of 1-80 characters")
	}

	card, err := t.store.Open(ctx, t.host, t.scope, args.Project, args.Text, title)
	if err != nil {
		return "", err
	}

	return cardLines(card, time.Now().UTC()), nil
}
