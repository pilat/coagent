package hooks

import "encoding/json"

type decision struct {
	command string
	block   bool
	reason  string
	context string
}

type hookOutput struct {
	Decision             string             `json:"decision"`
	Reason               string             `json:"reason"`
	Continue             *bool              `json:"continue"`
	HookSpecificOutput   hookSpecificOutput `json:"hookSpecificOutput"`
	UpdatedInput         json.RawMessage    `json:"updatedInput"`
	UpdatedToolOutput    json.RawMessage    `json:"updatedToolOutput"`
	UpdatedMCPToolOutput json.RawMessage    `json:"updatedMCPToolOutput"`
	SystemMessage        string             `json:"systemMessage"`
	StopReason           string             `json:"stopReason"`
}

type hookSpecificOutput struct {
	PermissionDecision       string          `json:"permissionDecision"`
	PermissionDecisionReason string          `json:"permissionDecisionReason"`
	AdditionalContext        string          `json:"additionalContext"`
	UpdatedInput             json.RawMessage `json:"updatedInput"`
	UpdatedToolOutput        json.RawMessage `json:"updatedToolOutput"`
	UpdatedMCPToolOutput     json.RawMessage `json:"updatedMCPToolOutput"`
}
