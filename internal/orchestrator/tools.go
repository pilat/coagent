package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/tool"
)

const (
	IDListSessions   = "list_sessions"
	IDSendToSession  = "send_to_session"
	IDSpawnSession   = "spawn_session"
	listDescription  = "List the owner's live root sessions, most recently active first (at most 20): id, name (project · title), state and last activity. Use the id with send_to_session."
	listParameters   = `{"type":"object","properties":{"filter":{"type":"string","description":"Optional word; lists only sessions whose name contains it, case-insensitive"}}}`
	sendDescription  = "Write a message to another of the owner's root sessions, including a stopped one, which this wakes. The message is plain text with no human authority: commands and skills are not executed. No reply or completion signal comes back to you; do not poll. Refused for killed sessions, subagents, this session itself, and sessions parked at a budget checkpoint."
	sendParameters   = `{"type":"object","properties":{"session_id":{"type":"integer","description":"Root session id from list_sessions"},"message":{"type":"string","description":"Plain text for that session's agent; it sees which session sent it"}},"required":["session_id","message"]}`
	spawnDescription = "Start a new root session in one of the owner's existing projects, optionally in a fresh git worktree. The prompt is delivered as a message from this session, not from the human. The new session is independent: its own budget, not your subagent, and the human sees it in /sessions. No reply or completion signal comes back; do not poll."
	spawnParameters  = `{"type":"object","properties":{"project":{"type":"string","description":"Existing project name, or its exact work directory"},"prompt":{"type":"string","description":"The first message for the new session; also names it"},"worktree":{"type":"string","description":"Create a fresh git worktree with this branch name off the remote default branch and run there"}},"required":["project","prompt"]}`
)

var (
	_ tool.Tool           = (*sessionTool)(nil)
	_ tool.TrailDeclarer  = (*sessionTool)(nil)
	_ tool.PromptDeclarer = (*sessionTool)(nil)
)

// Host revalidates the caller's capability and ownership for every operation.
type Host interface {
	ListSessions(ctx context.Context, callerID int64, filter string) ([]Entry, int, error)
	SendToSession(ctx context.Context, callerID, targetID int64, message string) (int64, error)
	SpawnSession(ctx context.Context, callerID int64, project, worktree, prompt string) (int64, string, error)
}

type Entry struct {
	ID          int64
	Name, State string
	Activity    time.Time
	Self        bool
}

type arguments struct {
	Filter    string `json:"filter"`
	SessionID int64  `json:"session_id"`
	Message   string `json:"message"`
	Project   string `json:"project"`
	Worktree  string `json:"worktree"`
	Prompt    string `json:"prompt"`
}

type sessionTool struct {
	host                             Host
	callerID                         int64
	id, trail, prompt, title, output string
	order                            int
	description, parameters          string
}

// NewTools binds the session capability to the caller; targets receive no tools.
func NewTools(host Host, sessionID int64) []tool.Tool {
	return []tool.Tool{
		&sessionTool{
			host, sessionID, IDListSessions,
			"filter", "list your root sessions", "", "", 1,
			listDescription, listParameters,
		},
		&sessionTool{
			host, sessionID, IDSendToSession,
			"message", "write to a session", "sent to #%d", "Message queued for session #%[1]d. ", 2,
			sendDescription, sendParameters,
		},
		&sessionTool{
			host, sessionID, IDSpawnSession,
			"prompt", "start a session in a project", "spawned #%d", "Started session #%[1]d in %[2]s. ", 3,
			spawnDescription, spawnParameters,
		},
	}
}

func (t *sessionTool) ID() string                        { return t.id }
func (t *sessionTool) Description() string               { return t.description }
func (t *sessionTool) Parameters() json.RawMessage       { return json.RawMessage(t.parameters) }
func (t *sessionTool) ParallelSafe() bool                { return false }
func (t *sessionTool) TrailArgument() tool.TrailArgument { return tool.TrailArgument{Param: t.trail} }

func (t *sessionTool) PromptDeclaration() tool.PromptDeclaration {
	return tool.PromptDeclaration{Group: tool.PromptGroupSessions, Order: t.order, Description: t.prompt}
}

func (t *sessionTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p arguments
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	switch {
	case t.id == IDSendToSession && p.SessionID <= 0:
		return nil, errors.New("session_id is required")
	case t.id == IDSendToSession && strings.TrimSpace(p.Message) == "":
		return nil, errors.New("message is required")
	case t.id == IDSpawnSession && strings.TrimSpace(p.Project) == "":
		return nil, errors.New("project is required")
	case t.id == IDSpawnSession && strings.TrimSpace(p.Prompt) == "":
		return nil, errors.New("prompt is required")
	}

	if t.id == IDListSessions {
		entries, total, err := t.host.ListSessions(ctx, t.callerID, p.Filter)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}

		return &tool.Result{Output: renderSessions(entries, total, p.Filter, time.Now())}, nil
	}

	id, where := t.callerID, ""
	var err error

	if t.id == IDSendToSession {
		id, err = t.host.SendToSession(ctx, id, p.SessionID, p.Message)
	} else {
		id, where, err = t.host.SpawnSession(ctx, id, p.Project, p.Worktree, p.Prompt)
	}

	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return &tool.Result{
		Title:  fmt.Sprintf(t.title, id),
		Output: fmt.Sprintf(t.output, id, where) + "No reply or completion signal comes back; do not poll.",
	}, nil
}

func renderSessions(entries []Entry, total int, filter string, now time.Time) string {
	if len(entries) == 0 {
		if filter != "" {
			return fmt.Sprintf("No sessions match %q.", filter)
		}

		return "No sessions."
	}

	lines := make([]string, 0, len(entries)+1)

	for _, entry := range entries {
		age := formatAge(now.Sub(entry.Activity))

		line := fmt.Sprintf("#%d %s — %s, active %s ago", entry.ID, entry.Name, entry.State, age)
		if entry.Self {
			line += " (this session)"
		}

		lines = append(lines, line)
	}

	if remaining := total - len(entries); remaining > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more; narrow with filter", remaining))
	}

	return strings.Join(lines, "\n")
}

func formatAge(age time.Duration) string {
	switch {
	case age >= 24*time.Hour:
		return fmt.Sprintf("%dd", int64(age/(24*time.Hour)))
	case age >= time.Hour:
		return fmt.Sprintf("%dh", int64(age/time.Hour))
	default:
		return fmt.Sprintf("%dm", max(int64(age/time.Minute), 0))
	}
}
