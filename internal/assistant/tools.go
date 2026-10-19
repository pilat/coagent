package assistant

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
)

const (
	toolSearch        = "search"
	toolAround        = "around"
	toolRead          = "read"
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

var toolSchemas = []llmwire.ToolSchema{
	schema(toolSearch, "Look: full-text search over every work session's requests and answers, your past "+
		"conversation, memory and focus. Search with word stems ending in * in both Russian and English "+
		"(\"кеш* cach* прав* permission*\"). Returns at most 10 hits with refs for around, read, "+
		"rewrite_memory and update_focus.",
		`{"query":{"type":"string","description":"Words or stems; a trailing * matches any ending."}}`, "query"),
	schema(toolAround, "Look: show a search hit in context. \"msg <id>\" shows that session's requests and "+
		"answers around it; \"line <id>\" shows your tape around it.",
		`{"ref":{"type":"string","description":"\"msg 5012\" or \"line 88\" from a search result."},`+
			`"count":{"type":"integer","minimum":1,"maximum":10,"description":"Entries on each side."}}`, "ref", "count"),
	schema(toolRead, "Look: the last requests and final answers of one work session, each whole.",
		`{"session":{"type":"integer","description":"Session id."},`+
			`"count":{"type":"integer","minimum":1,"maximum":60,"description":"How many entries."}}`,
		"session", "count"),
	schema(toolSessions, "Look: every work session you may address, most recently active first, 20 per page.",
		`{"offset":{"type":"integer","minimum":0,"description":"How many to skip."},`+
			`"project":{"type":"string","description":"Only this project key, optional."}}`, "offset"),
	schema(toolProjects, "Look: the known projects with their keys, places and last activity.", `{}`),
	schema(toolFocusList, "Look: every focus item, including this turn's edits.", `{}`),
	schema(toolOpen, "Do: start new work in a project with a self-contained instruction. Applied when the "+
		"turn ends; returns a handle usable in send and focus during this turn.",
		`{"project":{"type":"string","description":"An existing project key, or a new name for a scratch folder."},`+
			`"about":{"type":"string","description":"One line, why this session exists (1-200 characters)."},`+
			`"text":{"type":"string","description":"The instruction, in the user's language."}}`,
		"project", "about", "text"),
	schema(toolSend, "Do: continue a work session, finished or not. Applied when the turn ends.",
		`{"session":{"type":"string","description":"A session id like \"244\", or a handle from open."},`+
			`"text":{"type":"string","description":"The instruction, in the user's language."}}`, "session", "text"),
	schema(toolStop, "Do: stop a work session's current work. Applied when the turn ends.",
		`{"session":{"type":"integer","description":"Session id."}}`, "session"),
	schema(toolRemember, "Do: add a memory note (at most 300 characters). Without until it is permanent; "+
		"with until it leaves the desk after that date but stays searchable.",
		`{"text":{"type":"string"},"until":{"type":"string","description":"YYYY-MM-DD, optional."}}`, "text"),
	schema(toolRewriteMemory, "Do: replace a memory note's text and lifetime; omitting until makes it permanent.",
		`{"id":{"type":"string","description":"\"m12\", or a temporary id from remember."},`+
			`"text":{"type":"string"},"until":{"type":"string","description":"YYYY-MM-DD, optional."}}`, "id", "text"),
	schema(toolForgetMemory, "Do: delete a memory note; it can no longer be found.",
		`{"id":{"type":"string","description":"\"m12\", or a temporary id from remember."}}`, "id"),
	schema(toolFocus, "Do: add a focus item (at most 200 characters): what is being done now and what to tell "+
		"the user when it answers.",
		`{"text":{"type":"string"},"sessions":{"type":"array","items":{"type":"string"},`+
			`"description":"Session ids or handles this item waits on; may be empty."}}`, "text", "sessions"),
	schema(toolUpdateFocus, "Do: replace a focus item's text and sessions; omitted sessions are kept.",
		`{"id":{"type":"string","description":"\"f3\", or a temporary id from focus."},"text":{"type":"string"},`+
			`"sessions":{"type":"array","items":{"type":"string"}}}`, "id", "text"),
	schema(toolDoneFocus, "Do: remove a focus item that is finished or no longer wanted.",
		`{"id":{"type":"string","description":"\"f3\", or a temporary id from focus."}}`, "id"),
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

type stopArgs struct {
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

// sessionRef accepts a session as a JSON number or string, because models write both.
type sessionRef string

// Tools returns the schemas sent on every assistant call; their order is part of the prompt-cache key.
func Tools() []llmwire.ToolSchema {
	return toolSchemas
}

func (r *sessionRef) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*r = sessionRef(strings.TrimSpace(text))
		return nil
	}

	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return fmt.Errorf("session must be an id or a handle: %w", err)
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
