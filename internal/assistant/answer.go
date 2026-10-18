package assistant

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

const (
	sayField    = "say"
	userKind    = "user"
	observeKind = "observe"
)

// Answer holds one step's speech, actions and note edits; absent keys retain their value.
type Answer struct {
	Say    *string            `json:"say"`
	Do     []Action           `json:"do,omitempty"`
	Focus  *string            `json:"focus,omitempty"`
	Notes  map[string]*string `json:"notes,omitempty"`
	Forget [][]int64          `json:"forget,omitempty"`
}

// Action is a union: exactly one operation may be present.
type Action struct {
	Open *OpenAction `json:"open,omitempty"`
	Send *SendAction `json:"send,omitempty"`
	Read *ReadAction `json:"read,omitempty"`
	Stop *StopAction `json:"stop,omitempty"`
}

type OpenAction struct {
	Project string `json:"project"`
	About   string `json:"about"`
	Text    string `json:"text"`
}

type SendAction struct {
	Session int64  `json:"session"`
	Text    string `json:"text"`
}

type ReadAction struct {
	Session int64 `json:"session"`
	Lines   int   `json:"lines"`
}

type StopAction struct {
	Session int64 `json:"session"`
}

type noteEdit struct {
	Key  string  `json:"key"`
	Text *string `json:"text"`
}

type answerWire struct {
	Say    *string    `json:"say"`
	Do     []Action   `json:"do"`
	Focus  *string    `json:"focus"`
	Notes  []noteEdit `json:"notes"`
	Forget [][]int64  `json:"forget"`
}

// MarshalJSON emits list-shaped notes in key order, including when marshaling a value.
func (a Answer) MarshalJSON() ([]byte, error) {
	notes := make([]noteEdit, 0, len(a.Notes))
	for _, key := range slices.Sorted(maps.Keys(a.Notes)) {
		notes = append(notes, noteEdit{Key: key, Text: a.Notes[key]})
	}

	data, err := json.Marshal(answerWire{
		Say: a.Say, Do: append([]Action{}, a.Do...), Focus: a.Focus,
		Notes: notes, Forget: append([][]int64{}, a.Forget...),
	})
	if err != nil {
		return nil, fmt.Errorf("encode assistant answer: %w", err)
	}

	return data, nil
}

// UnmarshalJSON rejects unknown fields and duplicate note keys before any edits apply.
func (a *Answer) UnmarshalJSON(data []byte) error {
	var wire answerWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&wire); err != nil {
		return fmt.Errorf("decode answer fields: %w", err)
	}

	var notes map[string]*string
	if len(wire.Notes) > 0 {
		notes = make(map[string]*string, len(wire.Notes))
	}

	for _, edit := range wire.Notes {
		if _, exists := notes[edit.Key]; exists {
			return fmt.Errorf("note %s is written twice in one answer", edit.Key)
		}

		notes[edit.Key] = edit.Text
	}

	*a = Answer{Say: wire.Say, Do: wire.Do, Focus: wire.Focus, Notes: notes, Forget: wire.Forget}

	return nil
}

// FilterUnchangedNotes removes repeated writes while preserving deletes and open bindings.
func FilterUnchangedNotes(a *Answer, desk Desk) {
	current := noteMap(desk.Notes)
	for key, text := range a.Notes {
		scope, target, _ := strings.Cut(key, ".")
		if text == nil || ((scope == noteScopeWait || scope == noteScopeProject) && openedProject(*a, target)) {
			continue
		}

		if note, exists := current[key]; exists && note.Text == *text {
			delete(a.Notes, key)
		}
	}
}

// Parse tolerates wrappers but rejects unknown contract fields.
func Parse(text string) (Answer, error) {
	object, err := firstObject(text)
	if err != nil {
		return Answer{}, err
	}

	var answer Answer
	decoder := json.NewDecoder(strings.NewReader(object))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&answer); err != nil {
		return Answer{}, fmt.Errorf("decode assistant answer: %w", err)
	}

	if answer.Say != nil && strings.TrimSpace(*answer.Say) == "" {
		answer.Say = nil
	}

	return answer, nil
}

func firstObject(text string) (string, error) {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return "", errors.New("answer contains no JSON object")
	}

	depth, quoted, escaped := 0, false, false

	var out strings.Builder

	for i := start; i < len(text); i++ {
		char := text[i]

		if quoted {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				quoted = false
			}

			// Models often put raw line breaks inside strings; JSON needs them escaped.
			switch {
			case quoted && char == '\n':
				out.WriteString(`\n`)
			case quoted && char == '\r':
				out.WriteString(`\r`)
			case quoted && char == '\t':
				out.WriteString(`\t`)
			default:
				out.WriteByte(char)
			}

			continue
		}

		out.WriteByte(char)

		switch char {
		case '"':
			quoted = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return out.String(), nil
			}
		}
	}

	return "", errors.New("answer contains an unfinished JSON object")
}
