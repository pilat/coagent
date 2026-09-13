package configtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/tool"
)

// ConfigEditCommand is the exact user token whose turn grants edit authority.
const ConfigEditCommand = "/config"

const noConfigActivationMessage = "This change requires a current user message beginning with /config."

// editAuthorityDoc is the tool's own contract statement: the candidate replaces
// the whole application configuration, refusing an invalid one with nothing
// written. The restart contract is restartNotice in Description.
const editAuthorityDoc = "Replaces the complete application configuration with the supplied YAML document. " +
	"An invalid candidate is refused immediately with nothing written."

var (
	_ tool.Tool               = (*configEditTool)(nil)
	_ tool.ActivationDeclarer = (*configEditTool)(nil)
)

// DocumentStager is the raw-document staging surface config_edit consumes —
// structurally the configops.Service path, narrowed so the tool cannot stage
// typed ops.
type DocumentStager interface {
	StageDocument(candidate []byte) (*configops.Staged, configops.Verdict)
}

type configEditTool struct {
	stager DocumentStager
	stage  StageApply
}

type configEditParams struct {
	Document string `json:"document"`
}

// NewConfigEdit constructs the activation-gated full-document editing tool.
// The daemon registers it on ordinary root sessions and owns every staged call
// after StageApply accepts.
func NewConfigEdit(ops DocumentStager, stage StageApply) tool.Tool {
	return &configEditTool{stager: ops, stage: stage}
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
    "document": {"type": "string", "description": "The complete replacement configuration YAML. Credentials stay ${VAR} references to ` + secretsDisplayPath + ` — a literal is refused."}
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

	callID := tool.CallIDFromContext(ctx)
	if callID == "" {
		return nil, errors.New("no tool_call id to answer against")
	}

	if t.stager == nil || t.stage == nil {
		return nil, errors.New("this daemon cannot change its own configuration")
	}

	if p.Document == "" {
		return nil, errors.New("document is required")
	}

	staged, v := t.stager.StageDocument([]byte(p.Document))
	if v.Failed() {
		return nil, errors.New(v.Reason())
	}

	if !t.stage(callID, t.ID(), staged) {
		return nil, errors.New("another config change is already being applied — make one change at a time")
	}

	return nil, tool.ErrSuspend
}

// requireActivation revalidates the durable grant: the exact root session, this
// tool, the /config command, and the in-flight call.
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
