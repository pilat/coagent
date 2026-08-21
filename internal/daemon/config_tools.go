package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/tool"
)

// restartNotice tells the model the call does not answer immediately, or it
// treats the pause as a hang and retries.
const restartNotice = "Applying this restarts the daemon; you receive the verdict when it comes back. " +
	"A change that would break the daemon is refused instead, and the refusal comes back straight away."

// credentialDoc is the ${VAR}-only rule, stated where the model reads it.
const credentialDoc = "A ${VAR} reference to an entry in " + secretsDisplayPath + " — never the value itself. " +
	"A literal is refused. New secrets are created by request_secret, which only exists in a terminal chat."

var (
	_ tool.Tool = (*setProviderTool)(nil)
	_ tool.Tool = (*removeProviderTool)(nil)
	_ tool.Tool = (*setManagerTool)(nil)
	_ tool.Tool = (*removeManagerTool)(nil)
	_ tool.Tool = (*addModelTool)(nil)
	_ tool.Tool = (*removeModelTool)(nil)
	_ tool.Tool = (*setDefaultModelTool)(nil)
	_ tool.Tool = (*setModelTagsTool)(nil)
)

type (
	// configDeps is what every config tool needs: the daemon that owns the apply
	// pipeline, and the session waiting for its verdict.
	configDeps struct {
		svc       *svc
		sessionID int64
	}

	setProviderTool     struct{ configDeps }
	removeProviderTool  struct{ configDeps }
	setManagerTool      struct{ configDeps }
	removeManagerTool   struct{ configDeps }
	addModelTool        struct{ configDeps }
	removeModelTool     struct{ configDeps }
	setDefaultModelTool struct{ configDeps }
	setModelTagsTool    struct{ configDeps }

	setProviderParams struct {
		Name    string `json:"name"`
		Driver  string `json:"driver"`
		APIKey  string `json:"api_key"`
		SAFile  string `json:"sa_file"`
		BaseURL string `json:"base_url"`
		Catalog string `json:"catalog"`
	}

	setManagerParams struct {
		ID             string  `json:"id"`
		Driver         string  `json:"driver"`
		BotToken       string  `json:"bot_token"`
		AllowedUserIDs []int64 `json:"allowed_user_ids"`
		TargetChatID   int64   `json:"target_chat_id"`
	}

	nameParams struct {
		Name string `json:"name"`
	}

	removeModelParams struct {
		ID         string `json:"id"`
		NewDefault string `json:"new_default"`
	}
)

func newConfigTools(s *svc, sessionID int64) []tool.Tool {
	deps := configDeps{svc: s, sessionID: sessionID}

	return []tool.Tool{
		&setProviderTool{deps},
		&removeProviderTool{deps},
		&setManagerTool{deps},
		&removeManagerTool{deps},
		&addModelTool{deps},
		&removeModelTool{deps},
		&setDefaultModelTool{deps},
		&setModelTagsTool{deps},
	}
}

func (t *setProviderTool) ID() string { return tool.IDSetProvider }

func (t *setProviderTool) Description() string {
	return "Add an LLM provider, or change one that exists. " +
		"Leave api_key empty to keep the key an existing provider already has. " + restartNotice
}

func (t *setProviderTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "How models refer to this provider."},
    "driver": {"type": "string", "enum": ["anthropic", "openai", "openrouter", "google-sa"]},
    "api_key": {"type": "string", "description": ` + quote(credentialDoc) + `},
    "sa_file": {"type": "string", "description": "Service-account JSON path; google-sa only."},
    "base_url": {"type": "string", "description": "API endpoint; required for openrouter and google-sa."},
    "catalog": {"type": "string", "description": "models.dev section to resolve model metadata against. Leave empty to use the driver's default."}
  },
  "required": ["name", "driver"]
}`)
}

func (t *setProviderTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p setProviderParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse parameters: %w", err)
	}

	return t.apply(ctx, tool.IDSetProvider, configops.SetProvider(p.Name, config.ProviderEntry{
		Driver:  p.Driver,
		APIKey:  p.APIKey,
		SAFile:  p.SAFile,
		BaseURL: p.BaseURL,
		Catalog: p.Catalog,
	}))
}

func (t *removeProviderTool) ID() string { return tool.IDRemoveProvider }

func (t *removeProviderTool) Description() string {
	return "Delete a provider. Refused if it is the only one, or if any model still uses it — " +
		"remove those models first; nothing is deleted for you. " + restartNotice
}

func (t *removeProviderTool) Parameters() json.RawMessage { return nameSchema("Provider name.") }

func (t *removeProviderTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	p, err := parseName(params)
	if err != nil {
		return nil, err
	}

	return t.apply(ctx, tool.IDRemoveProvider, configops.RemoveProvider(p.Name))
}

func (t *setManagerTool) ID() string { return tool.IDSetManager }

func (t *setManagerTool) Description() string {
	return "Add a chat manager, or change one that exists. It is enabled by the call. " +
		"Leave bot_token empty to keep the token an existing manager already has. " + restartNotice
}

func (t *setManagerTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "Name for this manager, e.g. \"telegram-main\"."},
    "driver": {"type": "string", "enum": ["telegram"]},
    "bot_token": {"type": "string", "description": ` + quote(credentialDoc) + `},
    "allowed_user_ids": {"type": "array", "items": {"type": "integer"}, "description": "Numeric user ids allowed to talk to the bot."},
    "target_chat_id": {"type": "integer", "description": "The forum group's chat id, negative for a supergroup."}
  },
  "required": ["id", "driver"]
}`)
}

func (t *setManagerTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p setManagerParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse parameters: %w", err)
	}

	return t.apply(ctx, tool.IDSetManager, configops.SetManager(config.ManagerEntry{
		ID:             p.ID,
		Driver:         p.Driver,
		BotToken:       p.BotToken,
		AllowedUserIDs: p.AllowedUserIDs,
		TargetChatID:   p.TargetChatID,
	}))
}

func (t *removeManagerTool) ID() string { return tool.IDRemoveManager }

func (t *removeManagerTool) Description() string {
	return "Delete a chat manager. " + restartNotice
}

func (t *removeManagerTool) Parameters() json.RawMessage { return nameSchema("Manager id.") }

func (t *removeManagerTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	p, err := parseName(params)
	if err != nil {
		return nil, err
	}

	return t.apply(ctx, tool.IDRemoveManager, configops.RemoveManager(p.Name))
}

// apply is the shared half of every mutating config tool. A guard refusal is an
// ordinary tool error; a staged change suspends and answers after the restart.
func (d configDeps) apply(ctx context.Context, toolName string, op configops.Op) (*tool.Result, error) {
	callID := tool.CallIDFromContext(ctx)
	if callID == "" {
		return nil, errors.New("no tool_call id to answer against")
	}

	if d.svc.applier == nil {
		return nil, errors.New("this daemon cannot change its own configuration")
	}

	staged, v := d.svc.applier.Ops().Stage(op)
	if v.Failed() {
		return nil, errors.New(v.Reason())
	}

	if !d.svc.stageApply(d.sessionID, callID, toolName, staged) {
		return nil, errors.New("another config change is already being applied — make one change at a time")
	}

	return nil, tool.ErrSuspend
}

func parseName(params json.RawMessage) (nameParams, error) {
	var p nameParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("parse parameters: %w", err)
	}

	if p.Name == "" {
		return p, errors.New("name is required")
	}

	return p, nil
}

func nameSchema(doc string) json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {"name": {"type": "string", "description": ` + quote(doc) + `}},
  "required": ["name"]
}`)
}
