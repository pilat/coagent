package configtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/tool"
)

// restartNotice tells the model the call does not answer immediately, or it
// treats the pause as a hang and retries.
const restartNotice = "Applying this restarts the daemon; you receive the verdict when it comes back. " +
	"A change that would break the daemon is refused instead, and the refusal comes back straight away."

const secretsDisplayPath = "~/" + coagenthome.DirName + "/" + coagenthome.SecretsFileName

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
	// Stager is the semantic config operation slice the tool adapter consumes.
	Stager interface {
		Stage(ops ...configops.Op) (*configops.Staged, configops.Verdict)
	}

	// StageApply hands a validated candidate to the daemon that owns its durable
	// suspend and restart protocol. False means another apply already owns it.
	StageApply func(callID, toolName string, staged *configops.Staged) bool

	deps struct {
		ops   Stager
		stage StageApply
	}

	setProviderTool     struct{ deps }
	removeProviderTool  struct{ deps }
	removeManagerTool   struct{ deps }
	addModelTool        struct{ deps }
	removeModelTool     struct{ deps }
	setDefaultModelTool struct{ deps }
	setModelTagsTool    struct{ deps }

	setProviderParams struct {
		Name    string `json:"name"`
		Driver  string `json:"driver"`
		APIKey  string `json:"api_key"`
		SAFile  string `json:"sa_file"`
		BaseURL string `json:"base_url"`
		Catalog string `json:"catalog"`
	}

	nameParams struct {
		Name string `json:"name"`
	}

	removeModelParams struct {
		ID         string `json:"id"`
		NewDefault string `json:"new_default"`
	}
)

// New constructs the configuration-project tool surface. The daemon decides
// which session receives it and owns every staged call after StageApply accepts.
func New(ops Stager, stage StageApply) []tool.Tool {
	toolDeps := deps{ops: ops, stage: stage}

	return []tool.Tool{
		&setProviderTool{toolDeps},
		&removeProviderTool{toolDeps},
		&setManagerTool{toolDeps},
		&removeManagerTool{toolDeps},
		&addModelTool{toolDeps},
		&removeModelTool{toolDeps},
		&setDefaultModelTool{toolDeps},
		&setModelTagsTool{toolDeps},
	}
}

func (t *setProviderTool) ID() string { return tool.IDSetProvider }

func (t *setProviderTool) ParallelSafe() bool { return false }

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

func (t *removeProviderTool) ParallelSafe() bool { return false }

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

func (t *removeManagerTool) ID() string { return tool.IDRemoveManager }

func (t *removeManagerTool) ParallelSafe() bool { return false }

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
func (d deps) apply(ctx context.Context, toolName string, op configops.Op) (*tool.Result, error) {
	callID := tool.CallIDFromContext(ctx)
	if callID == "" {
		return nil, errors.New("no tool_call id to answer against")
	}

	if d.ops == nil || d.stage == nil {
		return nil, errors.New("this daemon cannot change its own configuration")
	}

	staged, v := d.ops.Stage(op)
	if v.Failed() {
		return nil, errors.New(v.Reason())
	}

	if !d.stage(callID, toolName, staged) {
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

func quote(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `""`
	}

	return string(encoded)
}
