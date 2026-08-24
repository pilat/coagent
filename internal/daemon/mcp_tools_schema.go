package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/tool"
)

const mcpStatusEnabled = "enabled"

// evict retires the server's pooled subprocess now (or on its last release)
// instead of letting it idle out the pool TTL.
func (d mcpDeps) evict(name string) {
	if d.pool != nil {
		d.pool.Evict(name)
	}
}

func (d mcpDeps) scopeOf(scope string) (mcpScope, error) {
	switch scope {
	case "global":
		return mcpScope{label: "global"}, nil
	case "project":
		if d.projectID == 0 {
			return mcpScope{}, errors.New("this session has no project, so it cannot use project scope")
		}

		projectID := d.projectID

		return mcpScope{projectID: &projectID, label: "project"}, nil
	default:
		return mcpScope{}, fmt.Errorf("scope must be \"global\" or \"project\", got %q", scope)
	}
}

func (d mcpDeps) parseNameScope(params json.RawMessage) (mcpNameParams, mcpScope, error) {
	var p mcpNameParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, mcpScope{}, fmt.Errorf("parse parameters: %w", err)
	}

	if p.Name == "" {
		return p, mcpScope{}, errors.New("name is required")
	}

	scope, err := d.scopeOf(p.Scope)

	return p, scope, err
}

// writeServerSection renders one scope. Env keys only — a value could be a token
// a user pasted despite the ${VAR} guidance.
func writeServerSection(b *strings.Builder, title string, defs []mcpstore.ServerDef) {
	fmt.Fprintf(b, "%s:\n", title)

	if len(defs) == 0 {
		b.WriteString("  (none)\n")

		return
	}

	for _, def := range defs {
		status := mcpStatusEnabled
		if !def.Enabled {
			status = "disabled"
		}

		fmt.Fprintf(b, "  %s [%s]: %s", def.Name, status, def.Command)

		if len(def.Args) > 0 {
			fmt.Fprintf(b, " %s", strings.Join(def.Args, " "))
		}

		if len(def.Env) > 0 {
			keys := make([]string, 0, len(def.Env))
			for k := range def.Env {
				keys = append(keys, k)
			}

			sort.Strings(keys)
			fmt.Fprintf(b, " (env: %s)", strings.Join(keys, ", "))
		}

		b.WriteString("\n")
	}
}

func nameScopeSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Registered server name."},
    "scope": {"type": "string", "enum": ["global", "project"], "description": ` +
		quote(scopeParamDoc) + `}
  },
  "required": ["name", "scope"]
}`)
}

func textResult(text string) *tool.Result {
	return &tool.Result{Output: text}
}

func quote(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `""`
	}

	return string(encoded)
}
