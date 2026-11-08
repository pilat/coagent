package session

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
)

const toolTrailWidth = 80

func renderToolTrail(calls []llmwire.ToolCall, reg *tool.Registry, workDir string) string {
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
					lines = append(lines, toolTrailLine(inner.Tool, inner.Params, reg, workDir))
				}

				continue
			}
		}

		lines = append(lines, toolTrailLine(call.Name, call.Arguments, reg, workDir))
	}

	return strings.Join(lines, "\n")
}

func toolTrailLine(name string, arguments json.RawMessage, reg *tool.Registry, workDir string) string {
	label := clipToolTrail(strings.NewReplacer("\n", "", "\r", "", "`", "").Replace(name))
	if strings.TrimSpace(label) == "" {
		label = "?"
	}

	line := "→ " + label

	argument := toolTrailArgument(reg, name)
	if argument.Param == "" {
		return line
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &params); err != nil {
		return line
	}

	var value string
	if err := json.Unmarshal(params[argument.Param], &value); err != nil {
		return line
	}

	value = logger.Redact(value)
	if argument.Path {
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

func toolTrailArgument(reg *tool.Registry, name string) tool.TrailArgument {
	if name == tool.IDTask {
		return tool.TrailArgument{Param: "description"}
	}

	if reg == nil {
		return tool.TrailArgument{}
	}

	if declarer, ok := reg.Get(name).(tool.TrailDeclarer); ok {
		return declarer.TrailArgument()
	}

	return tool.TrailArgument{}
}

func clipToolTrail(value string) string {
	runes := []rune(value)
	if len(runes) > toolTrailWidth {
		return string(runes[:toolTrailWidth-1]) + "…"
	}

	return value
}
