package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const (
	sessionsPage   = 20
	readMaxEntries = 60
	aroundMaxCount = 10
)

var toolTable = []toolEntry{
	{schema("search", "Look: full-text search over every work session's requests and answers, your past "+
		"conversation, memory and focus. Search with word stems ending in * in both Russian and English "+
		"(\"кеш* cach* прав* permission*\"). Returns at most 10 hits with refs for around, read_session, "+
		"rewrite_memory and update_focus.",
		`{"query":{"type":"string","description":"Words or stems; a trailing * matches any ending."}}`, "query",
	), false, false, (*assistantTool).search},
	{schema("around", "Look: show a message search hit among that session's requests and answers.",
		`{"ref":{"type":"string","description":"\"msg 5012\" from a search result."},`+
			`"count":{"type":"integer","minimum":1,"maximum":10,"description":"Entries on each side."}}`, "ref", "count"), true, false, (*assistantTool).around},
	{schema("read_session", "Look: the last requests and final answers of one work session, each whole.",
		`{"session":{"type":"string","description":"A session id like \"244\" or \"#244\"."},`+
			`"count":{"type":"integer","minimum":1,"maximum":60,"description":"How many entries."}}`,
		"session", "count"), true, false, (*assistantTool).readSession},
	{schema("sessions", "Look: every work session you may address, most recently active first, 20 per page.",
		`{"offset":{"type":"integer","minimum":0,"description":"How many to skip."},`+
			`"project":{"type":"string","description":"Only this project key, optional."}}`, "offset"), true, false, (*assistantTool).sessions},
	{schema("projects", "Look: the known projects with their keys, places and last activity.",
		`{}`), true, false, (*assistantTool).projects},
	{schema("focus_list", "Look: every live focus item.", `{}`), true, false, (*assistantTool).focusList},
	{schema("open", "Do: start new work immediately with a self-contained instruction; returns the real session id.",
		`{"project":{"type":"string","description":"An existing project key, or a new name for a scratch folder."},`+
			`"about":{"type":"string","description":"One line, why this session exists (1-80 characters)."},`+
			`"text":{"type":"string","description":"The instruction, in the user's language."},`+
			`"model":{"type":"string","description":"Configured model id; omitted uses the default model."},`+
			`"effort":{"type":"string","description":"Effort level; omitted uses the model's default."},`+
			`"worktree":{"type":"string","description":"New branch and worktree name in an existing repository project."}}`,
		"project", "about", "text"), false, false, (*assistantTool).open},
	{schema("send", "Do: immediately continue a work session, finished or stopped or still running.",
		`{"session":{"type":"string","description":"A session id like \"244\" or \"#244\"."},`+
			`"text":{"type":"string","description":"The instruction, in the user's language."}}`, "session", "text"), false, false, (*assistantTool).send},
	{schema("forward", "Do: deliver the user's current message to a work session word for word, as "+
		"their own input; you choose only the session.",
		`{"session":{"type":"string","description":"A session id like \"244\" or \"#244\"."}}`, "session",
	), false, true, (*assistantTool).forward},
	{schema("stop", "Do: stop a work session's current work immediately.",
		`{"session":{"type":"string","description":"A session id like \"244\" or \"#244\"."}}`, "session",
	), false, false, (*assistantTool).stop},
	{schema("kill",
		"Do: delete a work session for good at the user's request: its work stops and it leaves every list.",
		`{"session":{"type":"string","description":"A session id like \"244\" or \"#244\"."}}`, "session",
	), false, false, (*assistantTool).kill},
	{schema("switch",
		"Do: switch the user into a work session while they are talking to you; they return with /assistant.",
		`{"session":{"type":"string","description":"A session id like \"244\" or \"#244\"."}}`,
		"session"), false, false, (*assistantTool).switchChat},
	{schema("models", "Look: configured models in order, with default, effort choices, reasoning and catalog prices.",
		`{}`), true, false, (*assistantTool).models},
	{schema("set_model",
		"Do: set a work session's configured model and optional effort; omitted effort uses its default.",
		`{"session":{"type":"string","description":"A session id like \"244\" or \"#244\"."},`+
			`"model":{"type":"string"},"effort":{"type":"string"}}`,
		"session", "model"), false, false, (*assistantTool).setModel},
	{schema("remember", "Do: add a memory note (at most 300 characters). Without until it is permanent; "+
		"with until it leaves the desk after that date but stays searchable.",
		`{"text":{"type":"string"},"until":{"type":"string","description":"YYYY-MM-DD, optional."}}`, "text",
	), false, false, (*assistantTool).remember},
	{schema("rewrite_memory", "Do: replace a memory note's text and lifetime; omitting until makes it permanent.",
		`{"id":{"type":"string","description":"Memory id, for example \"m12\"."},`+
			`"text":{"type":"string"},"until":{"type":"string","description":"YYYY-MM-DD, optional."}}`, "id", "text"), false, false, (*assistantTool).rewriteMemory},
	{schema("forget_memory", "Do: delete a memory note; it can no longer be found.",
		`{"id":{"type":"string","description":"Memory id, for example \"m12\"."}}`, "id",
	), false, false, (*assistantTool).forgetMemory},
	{schema("focus", "Do: add a focus item (at most 200 characters): what is being done now and what to tell "+
		"the user when it answers.",
		`{"text":{"type":"string"},"sessions":{"type":"array","items":{"type":"string"},`+
			`"description":"Session ids this item waits on; may be empty."}}`, "text", "sessions"), false, false, (*assistantTool).addFocus},
	{schema("update_focus", "Do: replace a focus item's text and sessions; omitted sessions are kept.",
		`{"id":{"type":"string","description":"Focus id, for example \"f3\"."},"text":{"type":"string"},`+
			`"sessions":{"type":"array","items":{"type":"string"}}}`, "id", "text"), false, false, (*assistantTool).updateFocus},
	{schema("done_focus", "Do: remove a focus item that is finished or no longer wanted.",
		`{"id":{"type":"string","description":"Focus id, for example \"f3\"."}}`, "id",
	), false, false, (*assistantTool).doneFocus},
}

var _ tool.Tool = (*assistantTool)(nil)

// Host provides work-session lifecycle; Deliver's transaction rechecks authority.
type Host interface {
	// CreateWork commits the root and its first instruction together before starting it.
	CreateWork(ctx context.Context, owner string, spec WorkSpec, text string,
		enqueue func(tx *sql.Tx, id int64, attributes map[string]any) (*sessionstore.InboxInput, error),
	) (int64, string, error)
	// Deliver commits enqueue under the tree lock, then dispatches commands or wakes the session.
	Deliver(ctx context.Context, sessionID int64, text string,
		enqueue func(*sql.Tx, map[string]any) (*sessionstore.InboxInput, error),
	) error
	Stop(ctx context.Context, sessionID int64) error
	Kill(ctx context.Context, sessionID int64) error
	FocusChat(ctx context.Context, owner string, sessionID int64) error
	Models(ctx context.Context) ([]Model, error)
	SetModel(ctx context.Context, sessionID int64, model, effort string) error
}

type Model struct {
	ID, DisplayName, DefaultEffort string
	EffortLevels                   []string
	EffortAllowlist                []string // Empty means pass-through to the driver.
	Reasoning, Priced              bool
	InputPrice, OutputPrice        float64
}

type WorkSpec struct {
	ProjectID                                     int64
	Name, WorkDir, Title, Model, Effort, Worktree string
}

type toolEntry struct {
	schema   llmwire.ToolSchema
	parallel bool
	answers  bool
	run      func(*assistantTool, context.Context, json.RawMessage) (string, error)
}

type assistantTool struct {
	entry toolEntry
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
	Project  string  `json:"project"`
	About    string  `json:"about"`
	Text     string  `json:"text"`
	Model    string  `json:"model"`
	Effort   *string `json:"effort"`
	Worktree string  `json:"worktree"`
}

type sendArgs struct {
	Session sessionRef `json:"session"`
	Text    string     `json:"text"`
}

type forwardArgs struct {
	Session sessionRef `json:"session"`
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

type modelArgs struct {
	Session sessionRef `json:"session"`
	Model   string     `json:"model"`
	Effort  *string    `json:"effort"`
}

// sessionRef accepts numeric and string ids because models write both.
type sessionRef string

// Tools binds the assistant's immediate tools to one root and its manager.
func Tools(store *Store, scope Scope, host Host) []tool.Tool {
	tools := make([]tool.Tool, 0, len(toolTable))
	for _, entry := range toolTable {
		tools = append(tools, &assistantTool{entry: entry, store: store, scope: scope, host: host})
	}

	return tools
}

func (t *assistantTool) ID() string                  { return t.entry.schema.Name }
func (t *assistantTool) Description() string         { return t.entry.schema.Description }
func (t *assistantTool) Parameters() json.RawMessage { return t.entry.schema.Parameters }
func (t *assistantTool) ParallelSafe() bool          { return t.entry.parallel }

func (t *assistantTool) Execute(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
	text, err := t.entry.run(t, ctx, args)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.ID(), err)
	}

	return &tool.Result{Output: text, AnswersInput: t.entry.answers}, nil
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

func (r sessionRef) parse() (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(string(r), "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("session %q must be a session id", r)
	}

	return id, nil
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

func decodeArgs(name string, raw json.RawMessage, into any) error {
	if strings.TrimSpace(string(raw)) == "" {
		raw = []byte("{}")
	}

	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("arguments for %s: %w", name, err)
	}

	return nil
}
