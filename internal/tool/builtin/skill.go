package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/tool"
)

var _ tool.Tool = (*skillTool)(nil)

type skillParams struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

type skillTool struct {
	loader loader.Registry
}

func NewSkillTool(ldr loader.Registry) tool.Tool {
	return &skillTool{loader: ldr}
}

// RenderSkill renders the canonical conversation envelope for a skill invocation.
func RenderSkill(sk *loader.Skill, args string) string {
	return loader.RenderSkillInvocation(sk, args)
}

func (t *skillTool) ID() string         { return tool.IDSkill }
func (t *skillTool) ParallelSafe() bool { return false }

// Description builds a deterministic skill listing for the LLM prompt cache key.
func (t *skillTool) Description() string { return t.buildDescription() }

func (t *skillTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {
				"type": "string",
				"description": "The name of the skill to invoke"
			},
			"args": {
				"type": "string",
				"description": "Optional arguments to pass to the skill"
			}
		},
		"required": ["name"]
	}`)
}

func (t *skillTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p skillParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.Name == "" {
		return nil, errors.New("skill name is required")
	}

	sk, err := loader.ResolveModelInvocableSkill(t.loader, p.Name)
	if err != nil {
		return nil, fmt.Errorf("resolve skill: %w", err)
	}

	return &tool.Result{
		Title:  sk.Name,
		Output: RenderSkill(sk, p.Args),
		Metadata: map[string]any{
			"skill":     sk.Name,
			metaKeyPath: sk.Path,
			"args":      p.Args,
		},
		// The receipt commits atomically with this result; owner filtering
		// happens at the store boundary.
		DirectMessages: []string{loader.SkillReceipt(sk.Name)},
	}, nil
}

func (t *skillTool) buildDescription() string {
	if t.loader == nil {
		return "Invokes a skill. No skills are currently loaded."
	}

	skills := t.loader.ListModelInvocableSkills()
	if len(skills) == 0 {
		return "Invokes a skill. No skills are currently loaded."
	}

	var sb strings.Builder
	sb.WriteString("Invokes a loaded skill.\n\nAvailable skills:\n")

	for _, sk := range skills {
		fmt.Fprintf(&sb, "- %s", sk.Name)

		if description := sk.AnnouncementDescription(); description != "" {
			fmt.Fprintf(&sb, ": %s", description)
		}

		sb.WriteString("\n")
	}

	return sb.String()
}
