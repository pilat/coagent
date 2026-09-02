package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/tool"
)

// nextRunNotice is on every mutating result: a change lands in the tool set at the
// next run, never the current one.
const nextRunNotice = "Takes effect from the next run."

const scopeParamDoc = "Where the server lives: \"global\" (every project) or \"project\" (this project only). " +
	"A project server of the same name overrides the global one."

const envParamDoc = "Environment variables for the server. Secrets MUST be written as ${NAME} references " +
	"resolved from " + secretsDisplayPath + " at launch — never paste a literal token, it would be stored in plaintext."

// argsParamDoc closes the loophole envParamDoc alone leaves: many MCP servers also
// accept a credential as a flag, and args are stored and listed in full.
const argsParamDoc = "Command arguments. Never put a credential here — args are stored verbatim and shown by " +
	"mcp_list. A server that wants a key as a flag should read it from env instead."

var (
	_ tool.Tool = (*mcpAddTool)(nil)
	_ tool.Tool = (*mcpRemoveTool)(nil)
	_ tool.Tool = (*mcpEnableTool)(nil)
	_ tool.Tool = (*mcpDisableTool)(nil)
	_ tool.Tool = (*mcpListTool)(nil)
)

type (
	// mcpDeps is what every registry tool needs: the store to write, the project it
	// speaks for, and the pool to evict from on removal.
	mcpDeps struct {
		store     mcpstore.Store
		pool      mcp.Pool
		projectID int64
	}

	mcpAddTool     struct{ mcpDeps }
	mcpRemoveTool  struct{ mcpDeps }
	mcpEnableTool  struct{ mcpDeps }
	mcpDisableTool struct{ mcpDeps }
	mcpListTool    struct{ mcpDeps }

	mcpAddParams struct {
		Name    string            `json:"name"`
		Scope   string            `json:"scope"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	}

	mcpNameParams struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}

	// mcpScope names a registry scope. A nil projectID IS the global scope, so it
	// travels wrapped rather than as a bare nil return.
	mcpScope struct {
		projectID *int64
		label     string
	}
)

func newMCPTools(store mcpstore.Store, pool mcp.Pool, projectID int64) []tool.Tool {
	deps := mcpDeps{store: store, pool: pool, projectID: projectID}

	return []tool.Tool{
		&mcpAddTool{deps},
		&mcpRemoveTool{deps},
		&mcpEnableTool{deps},
		&mcpDisableTool{deps},
		&mcpListTool{deps},
	}
}

func (t *mcpAddTool) ID() string { return tool.IDMCPAdd }

func (t *mcpAddTool) ParallelSafe() bool { return false }

func (t *mcpAddTool) Description() string {
	return "Register an MCP server. " + nextRunNotice
}

func (t *mcpAddTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Server name; MCP tools appear as mcp__<name>__<tool>."},
    "scope": {"type": "string", "enum": ["global", "project"], "description": ` +
		quote(scopeParamDoc) + `},
    "command": {"type": "string", "description": "Executable to launch, e.g. \"npx\"."},
    "args": {"type": "array", "items": {"type": "string"}, "description": ` +
		quote(argsParamDoc) + `},
    "env": {"type": "object", "additionalProperties": {"type": "string"}, "description": ` +
		quote(envParamDoc) + `}
  },
  "required": ["name", "scope", "command"]
}`)
}

func (t *mcpAddTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p mcpAddParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse parameters: %w", err)
	}

	scope, err := t.scopeOf(p.Scope)
	if err != nil {
		return nil, err
	}

	if p.Name == "" || p.Command == "" {
		return nil, errors.New("name and command are required")
	}

	err = t.store.Add(ctx, scope.projectID, mcpstore.ServerDef{
		Name:    p.Name,
		Command: p.Command,
		Args:    p.Args,
		Env:     p.Env,
		Enabled: true,
	})
	if err != nil {
		return nil, fmt.Errorf("add mcp server: %w", err)
	}

	return textResult(fmt.Sprintf("Added MCP server %q in %s scope. %s", p.Name, scope.label, nextRunNotice)), nil
}

func (t *mcpRemoveTool) ID() string { return tool.IDMCPRemove }

func (t *mcpRemoveTool) ParallelSafe() bool { return false }

func (t *mcpRemoveTool) Description() string {
	return "Delete a registered MCP server. " + nextRunNotice
}

func (t *mcpRemoveTool) Parameters() json.RawMessage { return nameScopeSchema() }

func (t *mcpRemoveTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	p, scope, err := t.parseNameScope(params)
	if err != nil {
		return nil, err
	}

	if err := t.store.Remove(ctx, scope.projectID, p.Name); err != nil {
		return nil, fmt.Errorf("remove mcp server: %w", err)
	}

	t.evict(p.Name)

	return textResult(fmt.Sprintf("Removed MCP server %q from %s scope. %s", p.Name, scope.label, nextRunNotice)), nil
}

func (t *mcpEnableTool) ID() string { return tool.IDMCPEnable }

func (t *mcpEnableTool) ParallelSafe() bool { return false }

func (t *mcpEnableTool) Description() string {
	return "Switch a registered MCP server back on. " + nextRunNotice
}

func (t *mcpEnableTool) Parameters() json.RawMessage { return nameScopeSchema() }

func (t *mcpEnableTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	p, scope, err := t.parseNameScope(params)
	if err != nil {
		return nil, err
	}

	if err := t.store.SetEnabled(ctx, scope.projectID, p.Name, true); err != nil {
		return nil, fmt.Errorf("enable mcp server: %w", err)
	}

	return textResult(fmt.Sprintf("Enabled MCP server %q in %s scope. %s", p.Name, scope.label, nextRunNotice)), nil
}

func (t *mcpDisableTool) ID() string { return tool.IDMCPDisable }

func (t *mcpDisableTool) ParallelSafe() bool { return false }

func (t *mcpDisableTool) Description() string {
	return "Switch a registered MCP server off without deleting it. " + nextRunNotice
}

func (t *mcpDisableTool) Parameters() json.RawMessage { return nameScopeSchema() }

func (t *mcpDisableTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	p, scope, err := t.parseNameScope(params)
	if err != nil {
		return nil, err
	}

	if err := t.store.SetEnabled(ctx, scope.projectID, p.Name, false); err != nil {
		return nil, fmt.Errorf("disable mcp server: %w", err)
	}

	t.evict(p.Name)

	return textResult(fmt.Sprintf("Disabled MCP server %q in %s scope. %s", p.Name, scope.label, nextRunNotice)), nil
}

func (t *mcpListTool) ID() string { return tool.IDMCPList }

func (t *mcpListTool) ParallelSafe() bool { return false }

func (t *mcpListTool) Description() string {
	return "List registered MCP servers, global and project, enabled and disabled."
}

func (t *mcpListTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *mcpListTool) Execute(ctx context.Context, _ json.RawMessage) (*tool.Result, error) {
	globals, project, err := t.store.ListAll(ctx, t.projectID)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers: %w", err)
	}

	var b strings.Builder

	writeServerSection(&b, "Global", globals)
	b.WriteString("\n")
	writeServerSection(&b, "This project", project)

	return textResult(strings.TrimRight(b.String(), "\n")), nil
}
