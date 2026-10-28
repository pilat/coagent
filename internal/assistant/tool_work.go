package assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/llmwire"
)

func (t *assistantTool) work(ctx context.Context, call llmwire.ToolCall) (string, error) {
	if call.Name == toolOpen {
		var args openArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		args.Project = strings.TrimSpace(args.Project)
		args.Text = strings.TrimSpace(args.Text)
		args.About = strings.TrimSpace(args.About)

		if args.Project == "" || args.Text == "" {
			return "", errors.New("open needs a project and a text")
		}

		if args.About == "" || strings.ContainsAny(args.About, "\r\n") ||
			utf8.RuneCountInString(args.About) > aboutChars {
			return "", fmt.Errorf("about must be one line of 1-%d characters", aboutChars)
		}

		card, err := t.store.Open(ctx, t.host, t.scope, args.Project, args.Text, args.About)
		if err != nil {
			return "", err
		}

		return cardLines(card, time.Now().UTC()), nil
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
