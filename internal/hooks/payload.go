package hooks

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/tool"
)

func (h *Hooks) commonPayload(event string) commonPayload {
	return commonPayload{
		SessionID:     strconv.FormatInt(h.sessionID, 10),
		CWD:           h.workDir,
		HookEventName: event,
	}
}

func (h *Hooks) toolPayload(ctx context.Context, event string, call tool.Call, visible tool.HookCall) toolPayload {
	return toolPayload{
		commonPayload:   h.commonPayload(event),
		ToolName:        visible.Name.Name,
		CoagentToolName: call.Tool.ID(),
		ToolInput:       h.normalizeInput(visible.Input),
		ToolUseID:       tool.CallIDFromContext(ctx),
	}
}

func (h *Hooks) normalizeInput(input json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil || fields == nil {
		return input
	}

	if raw := fields["file_path"]; len(raw) == 0 || raw[0] != '"' {
		return input
	}

	var path string
	if json.Unmarshal(fields["file_path"], &path) != nil {
		return input
	}

	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := coagenthome.UserHome(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(h.workDir, path)
	}

	normalizedPath, err := json.Marshal(filepath.Clean(path))
	if err != nil {
		return input
	}

	fields["file_path"] = normalizedPath

	normalized, err := json.Marshal(fields)
	if err != nil {
		return input
	}

	return normalized
}
