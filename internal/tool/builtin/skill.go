package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/tool"
)

var batchCallHeaderPattern = regexp.MustCompile(`(?m)^=== [^\n]+ \(call \d+\) ===\n`)

// RenderedSkill is a canonical skill envelope found in conversation content.
type RenderedSkill struct {
	Name     string
	Envelope string
}

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
	body := strings.ReplaceAll(sk.Content, "$ARGUMENTS", args)
	if args != "" && !strings.Contains(sk.Content, "$ARGUMENTS") {
		body += "\n\nARGUMENTS: " + args
	}

	var output strings.Builder
	output.WriteString("<skill>\n")
	fmt.Fprintf(&output, "<name>%s</name>\n", html.EscapeString(sk.Name))

	if sk.Description != "" {
		fmt.Fprintf(&output, "<description>%s</description>\n", html.EscapeString(sk.Description))
	}

	output.WriteString("---\n")
	output.WriteString(body)

	if !strings.HasSuffix(body, "\n") {
		output.WriteString("\n")
	}

	output.WriteString("</skill>")

	return output.String()
}

// ExtractRenderedSkill extracts a canonical skill envelope from transport-prefixed content.
func ExtractRenderedSkill(content string) (string, string, bool) {
	skills := ExtractRenderedSkills(content)
	if len(skills) == 0 {
		return "", "", false
	}

	return skills[0].Name, skills[0].Envelope, true
}

// ExtractRenderedSkills extracts every canonical skill envelope from conversation content.
func ExtractRenderedSkills(content string) []RenderedSkill {
	const (
		startMarker = "<skill>\n<name>"
		nameEnd     = "</name>"
		endMarker   = "</skill>"
	)

	var skills []RenderedSkill

	for _, segment := range renderedSkillSegments(content, startMarker) {
		start := strings.Index(segment, startMarker)
		if start < 0 {
			continue
		}

		nameStart := start + len(startMarker)
		relativeNameEnd := strings.Index(segment[nameStart:], nameEnd)

		if relativeNameEnd < 0 {
			continue
		}

		name := html.UnescapeString(segment[nameStart : nameStart+relativeNameEnd])
		if name == "" {
			continue
		}

		envelopeEnd := len(segment)
		if end := strings.LastIndex(segment[start:], endMarker); end >= 0 {
			envelopeEnd = start + end + len(endMarker)
		}

		skills = append(skills, RenderedSkill{Name: name, Envelope: segment[start:envelopeEnd]})
	}

	return skills
}

func (t *skillTool) ID() string { return tool.IDSkill }

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

	sk := t.loader.GetSkill(p.Name)
	if sk == nil || !sk.IsModelInvocable() {
		skills := t.loader.ListModelInvocableSkills()
		names := make([]string, len(skills))

		for i, s := range skills {
			names[i] = s.Name
		}

		return nil, fmt.Errorf("skill unavailable: %s\nAvailable skills: %v", p.Name, names)
	}

	return &tool.Result{
		Title:  sk.Name,
		Output: RenderSkill(sk, p.Args),
		Metadata: map[string]any{
			"skill":     sk.Name,
			metaKeyPath: sk.Path,
			"args":      p.Args,
		},
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

func renderedSkillSegments(content, startMarker string) []string {
	headers := batchCallHeaderPattern.FindAllStringIndex(content, -1)
	if len(headers) == 0 || strings.Index(content, startMarker) < headers[0][0] {
		return []string{content}
	}

	segments := make([]string, 0, len(headers))
	for i, header := range headers {
		if !strings.HasPrefix(content[header[0]:header[1]], "=== skill ") {
			continue
		}

		end := len(content)
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}

		segments = append(segments, content[header[1]:end])
	}

	return segments
}
