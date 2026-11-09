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

var (
	_ tool.Tool           = (*skillTool)(nil)
	_ tool.TrailDeclarer  = (*skillTool)(nil)
	_ tool.PromptDeclarer = (*skillTool)(nil)
	_ tool.SkillLoader    = (*skillTool)(nil)
)

type skillParams struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

type skillTool struct {
	loader loader.SkillCatalog
}

func NewSkillTool(ldr *loader.Service) tool.Tool {
	t := &skillTool{}
	if ldr != nil {
		t.loader = ldr
	}

	return t
}

func (t *skillTool) ID() string         { return tool.IDSkill }
func (t *skillTool) ParallelSafe() bool { return false }
func (t *skillTool) SkillLoader()       {}

// Description builds a deterministic skill listing for the LLM prompt cache key.
func (t *skillTool) Description() string { return t.buildDescription() }

func (t *skillTool) TrailArgument() tool.TrailArgument {
	return tool.TrailArgument{Param: "name"}
}

func (t *skillTool) PromptDeclaration() tool.PromptDeclaration {
	return tool.PromptDeclaration{Group: tool.PromptGroupSkills, Order: 1, Description: "load domain knowledge"}
}

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
		Output: loader.RenderSkillInvocation(sk, p.Args),
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
