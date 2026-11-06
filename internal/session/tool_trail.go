package session

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

const toolTrailWidth = 80

func renderToolTrail(calls []llmwire.ToolCall, workDir string) string {
	var lines []string

	for _, call := range calls {
		if call.Name == "batch" {
			var batch struct {
				Calls []struct {
					Tool   string          `json:"tool"`
					Params json.RawMessage `json:"params"`
				} `json:"calls"`
			}
			if err := json.Unmarshal(call.Arguments, &batch); err == nil && len(batch.Calls) > 0 {
				for _, inner := range batch.Calls {
					lines = append(lines, toolTrailLine(inner.Tool, inner.Params, workDir))
				}

				continue
			}
		}

		lines = append(lines, toolTrailLine(call.Name, call.Arguments, workDir))
	}

	return strings.Join(lines, "\n")
}

func toolTrailLine(name string, arguments json.RawMessage, workDir string) string {
	label := clipToolTrail(strings.NewReplacer("\n", "", "\r", "", "`", "").Replace(name))
	if strings.TrimSpace(label) == "" {
		label = "?"
	}

	line := "→ " + label

	key := toolTrailArgument(name)
	if key == "" {
		return line
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &params); err != nil {
		return line
	}

	var value string
	if err := json.Unmarshal(params[key], &value); err != nil {
		return line
	}

	value = logger.Redact(value)
	if key == "file_path" || key == "path" {
		if rel, err := filepath.Rel(workDir, value); err == nil && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			value = rel
		}
	}

	value = strings.Join(strings.Fields(value), " ")

	value = clipToolTrail(strings.ReplaceAll(value, "`", "'"))
	if value == "" {
		return line
	}

	return line + ": `" + value + "`"
}

func toolTrailArgument(name string) string {
	switch name {
	case "bash":
		return "command"
	case "read", "write", "edit":
		return "file_path"
	case "grep", "glob":
		return "pattern"
	case "ls":
		return "path"
	case "webfetch":
		return "url"
	case "websearch":
		return "query"
	case "task":
		return "description"
	case "skill":
		return "name"
	default:
		return ""
	}
}

func clipToolTrail(value string) string {
	runes := []rune(value)
	if len(runes) > toolTrailWidth {
		return string(runes[:toolTrailWidth-1]) + "…"
	}

	return value
}
