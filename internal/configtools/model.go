package configtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/tool"
)

// defaultModelDoc states the one rule about model order a caller has to know.
const defaultModelDoc = "The first model in the list is the default the daemon starts sessions on."

type addModelParams struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

type idParams struct {
	ID string `json:"id"`
}

type setModelTagsParams struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
}

func (t *addModelTool) ID() string { return tool.IDAddModel }

func (t *addModelTool) ParallelSafe() bool { return false }

func (t *addModelTool) Description() string {
	return "Enable a model. Its metadata comes from the provider's catalog at startup, so an id the " +
		"catalog does not know keeps the daemon from starting and is rolled back. " +
		defaultModelDoc + " A model added here goes to the end. " + restartNotice
}

func (t *addModelTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "The provider's own model id, e.g. \"claude-sonnet-5\"."},
    "provider": {"type": "string", "description": "Name of a configured provider."}
  },
  "required": ["id", "provider"]
}`)
}

func (t *addModelTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p addModelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse parameters: %w", err)
	}

	return t.apply(ctx, tool.IDAddModel, configops.AddModel(config.ModelEntry{
		ID:       p.ID,
		Provider: p.Provider,
	}))
}

func (t *removeModelTool) ID() string { return tool.IDRemoveModel }

func (t *removeModelTool) ParallelSafe() bool { return false }

func (t *removeModelTool) Description() string {
	return "Disable a model. " + defaultModelDoc +
		" Removing the default is refused unless new_default names the model that takes its place. " + restartNotice
}

func (t *removeModelTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "Model id to remove."},
    "new_default": {"type": "string", "description": "Model id to promote to default. Required only when removing the current default."}
  },
  "required": ["id"]
}`)
}

func (t *removeModelTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p removeModelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse parameters: %w", err)
	}

	if p.ID == "" {
		return nil, errors.New("id is required")
	}

	return t.apply(ctx, tool.IDRemoveModel, configops.RemoveModel(p.ID, p.NewDefault))
}

func (t *setDefaultModelTool) ID() string { return tool.IDSetDefaultModel }

func (t *setDefaultModelTool) ParallelSafe() bool { return false }

func (t *setDefaultModelTool) Description() string {
	return "Make a configured model the default new sessions start on. " + restartNotice
}

func (t *setDefaultModelTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {"id": {"type": "string", "description": "Model id, already enabled."}},
  "required": ["id"]
}`)
}

func (t *setDefaultModelTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p idParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse parameters: %w", err)
	}

	if p.ID == "" {
		return nil, errors.New("id is required")
	}

	return t.apply(ctx, tool.IDSetDefaultModel, configops.SetDefaultModel(p.ID))
}

func (t *setModelTagsTool) ID() string { return tool.IDSetModelTags }

func (t *setModelTagsTool) ParallelSafe() bool { return false }

func (t *setModelTagsTool) Description() string {
	return "Replace a configured model's complete list of user-defined tags. Tags are lowercase letters, digits, _ or -; an empty list removes all tags. " + restartNotice
}

func (t *setModelTagsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "Configured model id."},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "Complete replacement tag list; empty removes all tags."}
  },
  "required": ["id", "tags"]
}`)
}

func (t *setModelTagsTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p setModelTagsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse parameters: %w", err)
	}

	if p.ID == "" {
		return nil, errors.New("id is required")
	}

	return t.apply(ctx, tool.IDSetModelTags, configops.SetModelTags(p.ID, p.Tags))
}
