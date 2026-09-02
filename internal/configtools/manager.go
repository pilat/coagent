package configtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/tool"
)

const managerFieldEnabled = "enabled"

// knownManagerFields is the complete set of accepted top-level keys.
var knownManagerFields = []string{
	"id", "driver", "enabled", "bot_token", "allowed_user_ids", "target_chat_id",
	"service_topic_name", "service_topic_icon_emoji_id", "session_topic_icon_emoji_id",
	"send_chunk_delay_ms", "poll_timeout_sec", "whisper",
}

// setManagerTool implements the presence-aware manager upsert.
// Ambiguous input fails before it can stage an apply, suspend, or restart.
type setManagerTool struct{ deps }

func (t *setManagerTool) ID() string { return tool.IDSetManager }

func (t *setManagerTool) ParallelSafe() bool { return false }

func (t *setManagerTool) Description() string {
	return "Add a chat manager, or change one that exists. " +
		"Omitted fields keep their existing values; present fields replace them. " +
		"A no-op call still restarts the daemon, which is the supported retry after " +
		"external Telegram capability or permission repair. " + restartNotice
}

func (t *setManagerTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "id": {"type": "string", "description": "Name for this manager, e.g. \"telegram-main\"."},
    "driver": {"type": "string", "enum": ["telegram"], "description": "Required when creating a manager; omit or repeat unchanged when updating."},
    "enabled": {"type": "boolean", "description": "Omit to keep the current state. Pass false to disable without removing config."},
    "bot_token": {"type": "string", "description": ` + quote(credentialDoc+" Omit to keep the reference an existing manager already has.") + `},
    "allowed_user_ids": {"type": "array", "items": {"type": "integer"}, "description": "Numeric user ids allowed to talk to the bot. A present array is the new complete list."},
    "target_chat_id": {"type": "integer", "description": "The group forum chat id. Omit for a private bot forum with exactly one allowed user. Cannot change on an existing manager."},
    "service_topic_name": {"type": "string", "description": "Name for the dedicated service topic. Empty resets to the default. Omit to keep the current name."},
    "service_topic_icon_emoji_id": {"type": "string", "description": "Icon for the service topic. Empty resets to the default. Omit to keep the current icon."},
    "session_topic_icon_emoji_id": {"type": "string", "description": "Default icon for future session topics. Empty resets to the default. Omit to keep the current default."},
    "send_chunk_delay_ms": {"type": "integer", "description": "Delay between message chunks in milliseconds. Zero resets to the default. Omit to keep the current value."},
    "poll_timeout_sec": {"type": "integer", "description": "Long-polling timeout in seconds. Zero resets to the default. Omit to keep the current value."},
    "whisper": {"type": ["object", "null"], "description": "Transcription provider. Pass null to remove; omit to keep.", "additionalProperties": false, "properties": {"provider": {"type": "string"}, "model": {"type": "string"}}, "required": ["provider", "model"]}
  },
  "required": ["id"]
}`)
}

func (t *setManagerTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	patch, err := parseManagerParams(params)
	if err != nil {
		return nil, err
	}

	return t.apply(ctx, tool.IDSetManager, configops.SetManagerPatch(patch))
}

// parseManagerParams rejects ambiguous input before constructing a manager patch.
// Only the "whisper" property itself accepts null.
func parseManagerParams(params json.RawMessage) (configops.ManagerPatch, error) {
	fields, err := readJSONObject(params)
	if err != nil {
		return configops.ManagerPatch{}, fmt.Errorf("parse parameters: %w", err)
	}

	var patch configops.ManagerPatch

	for key := range fields {
		if !slices.Contains(knownManagerFields, key) {
			return configops.ManagerPatch{}, fmt.Errorf("unknown field %q", key)
		}
	}

	id, ok, err := stringField(fields, "id")
	if err != nil {
		return configops.ManagerPatch{}, err
	}

	if !ok {
		return configops.ManagerPatch{}, errors.New("id is required")
	}

	if strings.TrimSpace(id) == "" {
		return configops.ManagerPatch{}, errors.New("id must be a non-empty string")
	}

	patch.ID = id
	if err := applyManagerPatchFields(fields, &patch); err != nil {
		return configops.ManagerPatch{}, err
	}

	return patch, nil
}
