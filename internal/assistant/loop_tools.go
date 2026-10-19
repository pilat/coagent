//nolint:wrapcheck // Tool errors are already model-facing domain errors.; nosemgrep: semgrep.coagent-no-preamble-before-package
package assistant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

const (
	sessionsPage    = 20
	readMaxEntries  = 60
	aroundMaxCount  = 10
	recordedResult  = "recorded; applied when the turn ends"
	ignoredToolCall = "not executed: no tool calls in this phase"
)

// runTools answers every call of one response in order, so the conversation stays provider-valid.
func (l *loop) runTools(ctx context.Context, t *turn, calls []llmwire.ToolCall, phase string) {
	for _, call := range calls {
		t.seq++
		text, err := l.runTool(ctx, t, call, phase)

		result := llmwire.Message{Role: llmwire.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: text}
		if err != nil {
			result.Content, result.ToolError = "error: "+logger.Redact(err.Error()), true
		}

		t.messages = append(t.messages, result)
	}
}

func ignoreTools(t *turn, calls []llmwire.ToolCall) {
	for _, call := range calls {
		t.messages = append(t.messages, llmwire.Message{
			Role: llmwire.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: ignoredToolCall, ToolError: true,
		})
	}
}

func (l *loop) runTool(ctx context.Context, t *turn, call llmwire.ToolCall, phase string) (string, error) {
	switch call.Name {
	case toolSearch, toolAround, toolRead, toolSessions, toolProjects, toolFocusList:
		return l.look(ctx, t, call)
	case toolOpen, toolSend, toolStop:
		if phase == phaseGuard {
			return "", errFinalReply
		}

		return l.work(ctx, t, call)
	case toolRemember, toolRewriteMemory, toolForgetMemory, toolFocus, toolUpdateFocus, toolDoneFocus:
		text, err := l.edit(ctx, t, call)
		if err == nil {
			t.lastEdit = t.seq
		}

		return text, err
	default:
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
}

func (l *loop) look(ctx context.Context, t *turn, call llmwire.ToolCall) (string, error) {
	switch call.Name {
	case toolSearch:
		var args searchArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		hits, err := l.deps.Sessions.Search(ctx, args.Query)
		if err != nil {
			return "", err
		}

		t.lastSearch = t.seq

		return renderHits(args.Query, hits), nil
	case toolAround:
		var args aroundArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		if args.Count < 1 || args.Count > aroundMaxCount {
			return "", fmt.Errorf("count must be between 1 and %d", aroundMaxCount)
		}

		return l.deps.Sessions.Around(ctx, args.Ref, args.Count)
	case toolRead:
		return l.read(ctx, t, call)
	case toolSessions:
		var args sessionsArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		cards, total, err := l.deps.Sessions.List(
			ctx,
			max(0, args.Offset),
			sessionsPage,
			strings.TrimSpace(args.Project),
		)
		if err != nil {
			return "", err
		}

		return renderSessions(cards, max(0, args.Offset), total, l.deps.Now()), nil
	case toolProjects:
		projects, err := l.deps.Sessions.Projects(ctx)
		if err != nil {
			return "", err
		}

		return renderProjects(projects, l.deps.Now()), nil
	default:
		return renderFocusList(t, l.deps.Now()), nil
	}
}

func (l *loop) read(ctx context.Context, t *turn, call llmwire.ToolCall) (string, error) {
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

	card, err := l.deps.Sessions.Target(ctx, id)
	if err != nil {
		return "", err
	}

	text, err := l.deps.Sessions.Read(ctx, id, args.Count)
	if err != nil {
		return "", err
	}

	if !slices.ContainsFunc(t.reads, func(read Card) bool { return read.ID == id }) {
		t.reads = append(t.reads, card)
	}

	return fmt.Sprintf("read %s (%d):\n%s", card.Project, card.ID, text), nil
}

func (l *loop) work(ctx context.Context, t *turn, call llmwire.ToolCall) (string, error) {
	switch call.Name {
	case toolOpen:
		var args openArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		e := &effect{
			kind: effectOpen, project: strings.TrimSpace(args.Project),
			about: strings.TrimSpace(args.About), text: strings.TrimSpace(args.Text),
		}
		if e.project == "" || e.text == "" {
			return "", errors.New("open needs a project and a text")
		}

		if e.about == "" || strings.ContainsAny(e.about, "\r\n") || utf8.RuneCountInString(e.about) > aboutChars {
			return "", fmt.Errorf("about must be one line of 1-%d characters", aboutChars)
		}

		if err := l.deps.Sessions.CheckProject(ctx, e.project); err != nil {
			return "", err
		}

		t.opened++
		e.handle = fmt.Sprintf("new-%d-%d", t.input.ID, t.opened)
		t.effects = append(t.effects, e)

		return fmt.Sprintf("%s; handle %s stands for the new session in send and focus", recordedResult, e.handle), nil
	case toolSend:
		var args sendArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		if strings.TrimSpace(args.Text) == "" {
			return "", errors.New("send needs a text")
		}

		e := &effect{kind: effectSend, text: strings.TrimSpace(args.Text)}
		if opened := t.openHandle(string(args.Session)); opened != nil {
			e.handle, e.project = opened.handle, opened.project
		} else if err := l.target(ctx, e, args.Session); err != nil {
			return "", err
		}

		t.effects = append(t.effects, e)

		return recordedResult, nil
	default:
		var args stopArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		e := &effect{kind: effectStop}
		if err := l.target(ctx, e, args.Session); err != nil {
			return "", err
		}

		t.effects = append(t.effects, e)

		return recordedResult, nil
	}
}

func (l *loop) target(ctx context.Context, e *effect, ref sessionRef) error {
	id, ok := ref.id()
	if !ok {
		return fmt.Errorf("session %q is neither a session id nor a handle opened in this turn", ref)
	}

	card, err := l.deps.Sessions.Target(ctx, id)
	if err != nil {
		return err
	}

	e.target = card

	return nil
}

func (l *loop) edit(ctx context.Context, t *turn, call llmwire.ToolCall) (string, error) {
	switch call.Name {
	case toolRemember, toolRewriteMemory, toolForgetMemory:
		var args memoryArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		switch call.Name {
		case toolRemember:
			ref, err := t.remember(args.Text, args.Until)
			if err != nil {
				return "", err
			}

			return fmt.Sprintf("%s; temporary id %s", recordedResult, ref), nil
		case toolRewriteMemory:
			return recordedResult, t.rewriteMemory(args.ID, args.Text, args.Until)
		default:
			return recordedResult, t.forgetMemory(args.ID)
		}
	default:
		var args focusArgs
		if err := decodeArgs(call, &args); err != nil {
			return "", err
		}

		if call.Name == toolDoneFocus {
			return recordedResult, t.doneFocus(args.ID)
		}

		sessions, err := l.focusSessions(ctx, t, args.Sessions)
		if err != nil {
			return "", err
		}

		if call.Name == toolUpdateFocus {
			return recordedResult, t.updateFocus(args.ID, args.Text, sessions, args.Sessions == nil)
		}

		ref, err := t.addFocus(args.Text, sessions)
		if err != nil {
			return "", err
		}

		return fmt.Sprintf("%s; temporary id %s", recordedResult, ref), nil
	}
}

// focusSessions accepts session ids the assistant may address and handles opened in this turn only.
func (l *loop) focusSessions(ctx context.Context, t *turn, refs []sessionRef) ([]string, error) {
	sessions := []string{}

	for _, ref := range refs {
		value := string(ref)
		if t.openHandle(value) == nil {
			id, ok := ref.id()
			if !ok {
				return nil, fmt.Errorf("session %q is neither a session id nor a handle opened in this turn", ref)
			}

			if _, err := l.deps.Sessions.Target(ctx, id); err != nil {
				return nil, err
			}

			value = strconv.FormatInt(id, 10)
		}

		if !slices.Contains(sessions, value) {
			sessions = append(sessions, value)
		}
	}

	return sessions, nil
}
