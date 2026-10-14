package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	sayField    = "say"
	userKind    = "user"
	observeKind = "observe"
)

// Answer contains replacement memory and the actions for one step.
type Answer struct {
	Think    string            `json:"think,omitempty"`
	Say      *string           `json:"say"`
	Do       []Action          `json:"do,omitempty"`
	Focus    string            `json:"focus"`
	Stickers []string          `json:"stickers"`
	Diary    map[string]string `json:"diary"`
	Forget   [][]int64         `json:"forget,omitempty"`
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

// Parse tolerates wrappers but rejects unknown or missing contract fields.
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

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(object), &fields); err != nil {
		return Answer{}, fmt.Errorf("decode assistant fields: %w", err)
	}

	for _, name := range []string{sayField, "focus", "stickers", "diary"} {
		value, exists := fields[name]
		if !exists || (name != sayField && string(value) == "null") {
			return Answer{}, fmt.Errorf("missing required field %s", name)
		}
	}

	answer.Think = string([]rune(answer.Think)[:min(400, len([]rune(answer.Think)))])
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
