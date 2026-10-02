package configapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/tool"
)

// ConfigEditCommand is the exact user token whose turn grants edit authority.
const ConfigEditCommand = "/config"

const noConfigActivationMessage = "This change requires a current user message beginning with /config."

// restartNotice tells the model the call does not answer immediately, or it
// treats the pause as a hang and retries.
const restartNotice = "Applying this restarts the daemon; you receive the verdict when it comes back. " +
	"A change that would break the daemon is refused instead, and the refusal comes back straight away."

const secretsDisplayPath = "~/" + coagenthome.DirName + "/" + coagenthome.SecretsFileName

const editAuthorityDoc = "Replaces the complete application configuration with the supplied YAML document. " +
	"Use only when the user explicitly requests a configuration change. A /config prefix alone is not a change request. " +
	"Answer configuration questions without calling this tool. Preserve every unrelated setting; never submit a fragment. " +
	"Credentials may be literal values or ${VAR} references to entries in " + secretsDisplayPath + ", " +
	"which the operator maintains by hand outside this protocol. " +
	"An invalid candidate is refused immediately with nothing written. " +
	"The sandbox is one ordered list of allow/deny path rules under sandbox.rules and sandbox.projects.<path>.rules; " +
	"the last matching rule decides. The current project's daemon-owned process output is always read-only after " +
	"all operator rules. A rule that buries a project's own writability is refused."

var (
	_ tool.Tool               = (*configEditTool)(nil)
	_ tool.ActivationDeclarer = (*configEditTool)(nil)
)

type configEditTool struct {
	service   *svc
	sessionID int64
}

type configEditParams struct {
	Document string `json:"document"`
}

func NewConfigEdit(sessionID int64, service Service) tool.Tool {
	return &configEditTool{sessionID: sessionID, service: service.(*svc)}
}

func (t *configEditTool) ID() string { return tool.IDConfigEdit }

func (t *configEditTool) ParallelSafe() bool { return false }

func (t *configEditTool) Description() string {
	return editAuthorityDoc + " " + restartNotice
}

func (t *configEditTool) ActivationCommands() []string { return []string{ConfigEditCommand} }

func (t *configEditTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "document": {"type": "string", "description": "The complete replacement configuration YAML. Credentials may be literal values; ${VAR} references to ` + secretsDisplayPath + ` keep working but their entries are maintained by hand outside this protocol."}
  },
  "required": ["document"]
}`)
}

func (t *configEditTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p configEditParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse parameters: %w", err)
	}

	if err := requireActivation(ctx); err != nil {
		return nil, err
	}
	grant, _ := tool.ActivationGrantFromContext(ctx)
	if grant.SessionID != t.sessionID {
		return nil, errors.New(noConfigActivationMessage)
	}

	callID := tool.CallIDFromContext(ctx)
	if callID == "" {
		return nil, errors.New("no tool_call id to answer against")
	}

	if t.service == nil {
		return nil, errors.New("this daemon cannot change its own configuration")
	}

	if p.Document == "" {
		return nil, errors.New("document is required")
	}

	staged, v := t.service.ops.StageDocument([]byte(p.Document))
	if v.Failed() {
		return nil, errors.New(v.Reason())
	}

	if !t.service.stageApply(t.sessionID, callID, staged) {
		return nil, errors.New("another config change is already being applied — make one change at a time")
	}

	return nil, tool.ErrSuspend
}

func requireActivation(ctx context.Context) error {
	grant, ok := tool.ActivationGrantFromContext(ctx)

	callID := tool.CallIDFromContext(ctx)
	if !ok || grant.ToolID != tool.IDConfigEdit || grant.Command != ConfigEditCommand ||
		grant.SessionID <= 0 || (grant.ToolCallID != "" && grant.ToolCallID != callID) {
		//nolint:staticcheck // Exact user-facing contract includes punctuation.
		return errors.New(noConfigActivationMessage)
	}

	return nil
}
