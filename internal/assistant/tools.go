package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

const (
	toolSearch        = "search"
	toolAround        = "around"
	toolRead          = "read_session"
	toolSessions      = "sessions"
	toolProjects      = "projects"
	toolFocusList     = "focus_list"
	toolOpen          = "open"
	toolSend          = "send"
	toolStop          = "stop"
	toolRemember      = "remember"
	toolRewriteMemory = "rewrite_memory"
	toolForgetMemory  = "forget_memory"
	toolFocus         = "focus"
	toolUpdateFocus   = "update_focus"
	toolDoneFocus     = "done_focus"
)

const (
	sessionsPage   = 20
	readMaxEntries = 60
	aroundMaxCount = 10
	focusTextChars = 200
	focusLiveItems = 30
	aboutChars     = 200
)

var toolSchemas = []llmwire.ToolSchema{
	schema(toolSearch, "Look: full-text search over every work session's requests and answers, your past "+
		"conversation, memory and focus. Search with word stems ending in * in both Russian and English "+
		"(\"кеш* cach* прав* permission*\"). Returns at most 10 hits with refs for around, read_session, "+
		"rewrite_memory and update_focus.",
		`{"query":{"type":"string","description":"Words or stems; a trailing * matches any ending."}}`, "query"),
	schema(toolAround, "Look: show a message search hit among that session's requests and answers.",
		`{"ref":{"type":"string","description":"\"msg 5012\" from a search result."},`+
			`"count":{"type":"integer","minimum":1,"maximum":10,"description":"Entries on each side."}}`, "ref", "count"),
	schema(toolRead, "Look: the last requests and final answers of one work session, each whole.",
		`{"session":{"type":"integer","description":"Session id."},`+
			`"count":{"type":"integer","minimum":1,"maximum":60,"description":"How many entries."}}`,
		"session", "count"),
	schema(toolSessions, "Look: every work session you may address, most recently active first, 20 per page.",
		`{"offset":{"type":"integer","minimum":0,"description":"How many to skip."},`+
			`"project":{"type":"string","description":"Only this project key, optional."}}`, "offset"),
	schema(toolProjects, "Look: the known projects with their keys, places and last activity.", `{}`),
	schema(toolFocusList, "Look: every live focus item.", `{}`),
	schema(toolOpen, "Do: start new work immediately with a self-contained instruction; returns the real session id.",
		`{"project":{"type":"string","description":"An existing project key, or a new name for a scratch folder."},`+
			`"about":{"type":"string","description":"One line, why this session exists (1-200 characters)."},`+
			`"text":{"type":"string","description":"The instruction, in the user's language."}}`,
		"project", "about", "text"),
	schema(toolSend, "Do: immediately continue a work session, finished or stopped or still running.",
		`{"session":{"type":"string","description":"A session id like \"244\"."},`+
			`"text":{"type":"string","description":"The instruction, in the user's language."}}`, "session", "text"),
	schema(toolStop, "Do: stop a work session's current work immediately.",
		`{"session":{"type":"integer","description":"Session id."}}`, "session"),
	schema(toolRemember, "Do: add a memory note (at most 300 characters). Without until it is permanent; "+
		"with until it leaves the desk after that date but stays searchable.",
		`{"text":{"type":"string"},"until":{"type":"string","description":"YYYY-MM-DD, optional."}}`, "text"),
	schema(toolRewriteMemory, "Do: replace a memory note's text and lifetime; omitting until makes it permanent.",
		`{"id":{"type":"string","description":"Memory id, for example \"m12\"."},`+
			`"text":{"type":"string"},"until":{"type":"string","description":"YYYY-MM-DD, optional."}}`, "id", "text"),
	schema(toolForgetMemory, "Do: delete a memory note; it can no longer be found.",
		`{"id":{"type":"string","description":"Memory id, for example \"m12\"."}}`, "id"),
	schema(toolFocus, "Do: add a focus item (at most 200 characters): what is being done now and what to tell "+
		"the user when it answers.",
		`{"text":{"type":"string"},"sessions":{"type":"array","items":{"type":"string"},`+
			`"description":"Session ids this item waits on; may be empty."}}`, "text", "sessions"),
	schema(toolUpdateFocus, "Do: replace a focus item's text and sessions; omitted sessions are kept.",
		`{"id":{"type":"string","description":"Focus id, for example \"f3\"."},"text":{"type":"string"},`+
			`"sessions":{"type":"array","items":{"type":"string"}}}`, "id", "text"),
	schema(toolDoneFocus, "Do: remove a focus item that is finished or no longer wanted.",
		`{"id":{"type":"string","description":"Focus id, for example \"f3\"."}}`, "id"),
}

var _ tool.Tool = (*assistantTool)(nil)

// Host provides work-session lifecycle; Deliver's transaction rechecks authority.
type Host interface {
	CreateWork(ctx context.Context, owner string, projectID int64, name, workDir string) (int64, error)
	// Deliver commits enqueue under the session's tree lock, then wakes it.
	Deliver(ctx context.Context, sessionID int64, text string, enqueue func(*sql.Tx) error) error
	Stop(ctx context.Context, sessionID int64) error
}

type assistantTool struct {
	spec  llmwire.ToolSchema
	store *Store
	scope Scope
	host  Host
}

type searchArgs struct {
	Query string `json:"query"`
}

type aroundArgs struct {
	Ref   string `json:"ref"`
	Count int    `json:"count"`
}

type readArgs struct {
	Session sessionRef `json:"session"`
	Count   int        `json:"count"`
}

type sessionsArgs struct {
	Offset  int    `json:"offset"`
	Project string `json:"project"`
}

type openArgs struct {
	Project string `json:"project"`
	About   string `json:"about"`
	Text    string `json:"text"`
}

type sendArgs struct {
	Session sessionRef `json:"session"`
	Text    string     `json:"text"`
}

type memoryArgs struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Until string `json:"until"`
}

type focusArgs struct {
	ID       string       `json:"id"`
	Text     string       `json:"text"`
	Sessions []sessionRef `json:"sessions"`
}

// sessionRef accepts numeric and string ids because models write both.
type sessionRef string

// Tools binds the assistant's immediate tools to one root and its manager.
func Tools(store *Store, scope Scope, host Host) []tool.Tool {
	tools := make([]tool.Tool, 0, len(toolSchemas))
	for _, spec := range toolSchemas {
		tools = append(tools, &assistantTool{spec: spec, store: store, scope: scope, host: host})
	}

	return tools
}

func (t *assistantTool) ID() string                  { return t.spec.Name }
func (t *assistantTool) Description() string         { return t.spec.Description }
func (t *assistantTool) Parameters() json.RawMessage { return t.spec.Parameters }
func (t *assistantTool) ParallelSafe() bool {
	switch t.ID() {
	case toolAround, toolRead, toolSessions, toolProjects, toolFocusList:
		return true
	default:
		return false
	}
}

func (t *assistantTool) Execute(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
	call := llmwire.ToolCall{Name: t.ID(), Arguments: args}
	var text string
	var err error

	switch t.ID() {
	case toolSearch, toolAround, toolRead, toolSessions, toolProjects, toolFocusList:
		text, err = t.look(ctx, call)
	case toolOpen, toolSend, toolStop:
		text, err = t.work(ctx, call)
	default:
		text, err = t.edit(ctx, call)
	}

	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.ID(), err)
	}

	return &tool.Result{Output: text}, nil
}

func (r *sessionRef) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*r = sessionRef(strings.TrimSpace(text))
		return nil
	}

	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return fmt.Errorf("session must be an id: %w", err)
	}

	*r = sessionRef(number.String())

	return nil
}

func (r sessionRef) id() (int64, bool) {
	id, err := strconv.ParseInt(string(r), 10, 64)
	return id, err == nil && id > 0
}

func schema(name, description, properties string, required ...string) llmwire.ToolSchema {
	quoted := make([]string, 0, len(required))
	for _, field := range required {
		quoted = append(quoted, strconv.Quote(field))
	}

	return llmwire.ToolSchema{
		Name:        name,
		Description: description,
		Parameters: json.RawMessage(`{"type":"object","properties":` + properties +
			`,"required":[` + strings.Join(quoted, ",") + `]}`),
	}
}

func decodeArgs(call llmwire.ToolCall, into any) error {
	raw := call.Arguments
	if strings.TrimSpace(string(raw)) == "" {
		raw = []byte("{}")
	}

	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("arguments for %s: %w", call.Name, err)
	}

	return nil
}
