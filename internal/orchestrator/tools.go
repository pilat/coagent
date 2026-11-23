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
	IDListSessions  = "list_sessions"
	IDSendToSession = "send_to_session"
	IDSpawnSession  = "spawn_session"

	listDescription  = "List the owner's live root sessions, most recently active first (at most 20): id, name (project · title), state and last activity. Use the id with send_to_session. Sessions marked \"spawned by you\" are the ones stop_session and kill_session accept."
	listParameters   = `{"type":"object","properties":{"filter":{"type":"string","description":"Optional word; lists only sessions whose name contains it, case-insensitive"}}}`
	sendDescription  = "Write a message to another of the owner's root sessions, including a stopped one, which this wakes. The message is plain text with no human authority: commands and skills are not executed. When that session's turn is over you get one signal line; then read it with read_session. Do not poll. Refused for killed sessions, subagents, this session itself, and sessions parked at a budget checkpoint."
	sendParameters   = `{"type":"object","properties":{"session_id":{"type":"integer","description":"Root session id from list_sessions"},"message":{"type":"string","description":"Plain text for that session's agent; it sees which session sent it"}},"required":["session_id","message"]}`
	spawnDescription = "Start a new root session in one of the owner's existing projects, optionally in a fresh git worktree. The prompt is delivered as a message from this session, not from the human. The new session is independent: its own budget, not your subagent, and the human sees it in /sessions. You get one signal when its first turn is over; then read it with read_session. Do not poll."
	spawnParameters  = `{"type":"object","properties":{"project":{"type":"string","description":"Existing project name, or its exact work directory"},"prompt":{"type":"string","description":"The first message for the new session; also names it"},"worktree":{"type":"string","description":"Create a fresh git worktree with this branch name off the remote default branch and run there"},"model":{"type":"string","description":"Optional tagged model ID from the models the task tool lists for subagents (not inherit); omit to start on the daemon's default model"},"effort":{"type":"string","description":"Optional reasoning effort for the new session. Set only when the user explicitly names a level; omit to take the model's default. If the model rejects it, the error lists accepted levels."}},"required":["project","prompt"]}`

	sendAcknowledgementPrefix      = "Message queued for session #%d. "
	subscribeAcknowledgementPrefix = "Subscribed to session #%d. "
	sendAcknowledgement            = sendAcknowledgementPrefix + "You get one signal when its turn is over; then read it with read_session."
	spawnAcknowledgement           = "Started session #%d in %s. You get one signal when its first turn is over; then read it with read_session."
	subscribeAcknowledgement       = subscribeAcknowledgementPrefix + "You get one signal per turn outcome, and none again until you read it with read_session."
	unreadSignalAcknowledgement    = "A signal for session #%d is still unread. Read it with read_session (session_id %d) before another signal can come."
	sendUnreadAcknowledgement      = sendAcknowledgementPrefix + unreadSignalAcknowledgement
	subscribeUnreadAcknowledgement = subscribeAcknowledgementPrefix + unreadSignalAcknowledgement
	unsubscribeAcknowledgement     = "Unsubscribed from session #%d."
	notSubscribedAcknowledgement   = "You were not subscribed to session #%d."
)

var (
	_ tool.Tool           = (*sessionTool)(nil)
	_ tool.TrailDeclarer  = (*sessionTool)(nil)
	_ tool.PromptDeclarer = (*sessionTool)(nil)
)

// Host revalidates the caller's capability and ownership for every operation.
type Host interface {
	ListSessions(ctx context.Context, callerID int64, filter string) ([]Entry, int, error)
	SendToSession(ctx context.Context, callerID, targetID int64, message string) (int64, bool, error)
	SpawnSession(ctx context.Context, callerID int64, request SpawnRequest) (int64, string, error)
	ReadSession(ctx context.Context, callerID, targetID int64, latest bool) (Page, error)
	Subscribe(ctx context.Context, callerID, targetID int64) (int64, bool, error)
	Unsubscribe(ctx context.Context, callerID, targetID int64) (int64, bool, error)
	StopSession(ctx context.Context, callerID, targetID int64) (Ended, error)
	KillSession(ctx context.Context, callerID, targetID int64) (Ended, error)
}

type Entry struct {
	ID                           int64
	Name                         string
	State                        string
	Activity                     time.Time
	Self                         bool
	Spawned                      bool
	Subscribed, Armed, Signalled bool
}

// SpawnRequest is one spawn_session call. An empty Worktree, Model or Effort means no worktree,
// the default model, or that model's default level.
type SpawnRequest struct {
	Project  string
	Worktree string
	Prompt   string
	Model    string
	Effort   string
}

type arguments struct {
	Filter    string `json:"filter"`
	SessionID int64  `json:"session_id"`
	Message   string `json:"message"`
	Project   string `json:"project"`
	Worktree  string `json:"worktree"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Latest    bool   `json:"latest"`
}

type sessionTool struct {
	host              Host
	callerID          int64
	id                string
	description       string
	parameters        string
	trailParam        string
	promptDescription string
	promptOrder       int
}

// NewTools binds the session capability to the caller; targets receive no tools.
func NewTools(host Host, sessionID int64) []tool.Tool {
	return append([]tool.Tool{
		&sessionTool{
			host:              host,
			callerID:          sessionID,
			id:                IDListSessions,
			description:       listDescription,
			parameters:        listParameters,
			trailParam:        "filter",
			promptDescription: "list your root sessions",
			promptOrder:       1,
		},
		&sessionTool{
			host:              host,
			callerID:          sessionID,
			id:                IDSendToSession,
			description:       sendDescription,
			parameters:        sendParameters,
			trailParam:        "message",
			promptDescription: "write to a session",
			promptOrder:       2,
		},
		&sessionTool{
			host:              host,
			callerID:          sessionID,
			id:                IDSpawnSession,
			description:       spawnDescription,
			parameters:        spawnParameters,
			trailParam:        "prompt",
			promptDescription: "start a session in a project",
			promptOrder:       3,
		},
		untrustedTool{sessionTool: &sessionTool{
			host:              host,
			callerID:          sessionID,
			id:                IDReadSession,
			description:       readDescription,
			parameters:        readParameters,
			promptDescription: "read a session's chat",
			promptOrder:       4,
		}},
	}, append(newWatchTools(host, sessionID), newEndTools(host, sessionID)...)...)
}

func (t *sessionTool) ID() string                  { return t.id }
func (t *sessionTool) Description() string         { return t.description }
func (t *sessionTool) Parameters() json.RawMessage { return json.RawMessage(t.parameters) }
func (t *sessionTool) ParallelSafe() bool          { return false }
func (t *sessionTool) TrailArgument() tool.TrailArgument {
	return tool.TrailArgument{Param: t.trailParam}
}

func (t *sessionTool) PromptDeclaration() tool.PromptDeclaration {
	return tool.PromptDeclaration{
		Group:       tool.PromptGroupSessions,
		Order:       t.promptOrder,
		Description: t.promptDescription,
	}
}

func (t *sessionTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var args arguments
	if err := json.Unmarshal(params, &args); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	switch {
	case needsSessionID(t.id) && args.SessionID <= 0:
		return nil, errors.New("session_id is required")
	case t.id == IDSendToSession && strings.TrimSpace(args.Message) == "":
		return nil, errors.New("message is required")
	case t.id == IDSpawnSession && strings.TrimSpace(args.Project) == "":
		return nil, errors.New("project is required")
	case t.id == IDSpawnSession && strings.TrimSpace(args.Prompt) == "":
		return nil, errors.New("prompt is required")
	}

	switch t.id {
	case IDListSessions:
		return t.list(ctx, args.Filter)
	case IDSendToSession:
		return t.send(ctx, args)
	case IDReadSession:
		return t.read(ctx, args)
	case IDSubscribe:
		return t.subscribe(ctx, args.SessionID)
	case IDUnsubscribe:
		return t.unsubscribe(ctx, args.SessionID)
	case IDStopSession:
		return t.stop(ctx, args.SessionID)
	case IDKillSession:
		return t.kill(ctx, args.SessionID)
	default:
		return t.spawn(ctx, args)
	}
}

func (t *sessionTool) list(ctx context.Context, filter string) (*tool.Result, error) {
	entries, total, err := t.host.ListSessions(ctx, t.callerID, filter)
	if err != nil {
		return nil, fmt.Errorf("%w", err) // The executor already names the tool.
	}

	return &tool.Result{Output: renderSessions(entries, total, filter, time.Now())}, nil
}

func (t *sessionTool) send(ctx context.Context, args arguments) (*tool.Result, error) {
	targetID, unread, err := t.host.SendToSession(ctx, t.callerID, args.SessionID, args.Message)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	output := fmt.Sprintf(sendAcknowledgement, targetID)
	if unread {
		output = fmt.Sprintf(sendUnreadAcknowledgement, targetID, targetID, targetID)
	}

	return &tool.Result{
		Title:  fmt.Sprintf("sent to #%d", targetID),
		Output: output,
	}, nil
}

func (t *sessionTool) spawn(ctx context.Context, args arguments) (*tool.Result, error) {
	sessionID, project, err := t.host.SpawnSession(ctx, t.callerID, SpawnRequest{
		Project:  args.Project,
		Worktree: args.Worktree,
		Prompt:   args.Prompt,
		Model:    args.Model,
		Effort:   args.Effort,
	})
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return &tool.Result{
		Title:  fmt.Sprintf("spawned #%d", sessionID),
		Output: fmt.Sprintf(spawnAcknowledgement, sessionID, project),
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
		line := fmt.Sprintf(
			"#%d %s — %s, active %s ago",
			entry.ID,
			entry.Name,
			entry.State,
			formatAge(now.Sub(entry.Activity)),
		)
		if state := entryMarkers(entry); state != "" {
			line += "; " + state
		}

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
