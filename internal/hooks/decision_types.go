package hooks

import "encoding/json"

type decision struct {
	command string
	block   bool
	reason  string
	context string
}

// Input-only lowercase tags retain camelCase matching under the local JSON tag rule.
type hookOutput struct {
	Decision             string             `json:"decision"`
	Reason               string             `json:"reason"`
	Continue             *bool              `json:"continue"`
	HookSpecificOutput   hookSpecificOutput `json:"hookspecificoutput"`
	UpdatedInput         json.RawMessage    `json:"updatedinput"`
	UpdatedToolOutput    json.RawMessage    `json:"updatedtooloutput"`
	UpdatedMCPToolOutput json.RawMessage    `json:"updatedmcptooloutput"`
	SystemMessage        string             `json:"systemmessage"`
	StopReason           string             `json:"stopreason"`
}

type hookSpecificOutput struct {
	PermissionDecision       string          `json:"permissiondecision"`
	PermissionDecisionReason string          `json:"permissiondecisionreason"`
	AdditionalContext        string          `json:"additionalcontext"`
	UpdatedInput             json.RawMessage `json:"updatedinput"`
	UpdatedToolOutput        json.RawMessage `json:"updatedtooloutput"`
	UpdatedMCPToolOutput     json.RawMessage `json:"updatedmcptooloutput"`
}
