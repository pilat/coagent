package sessionbuild

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/mcpstore"
)

const toolLiteralRulePath = "../../.semgrep/coagent.yml"

type toolLiteralRuleConfig struct {
	Rules []struct {
		ID       string                 `yaml:"id"`
		Patterns []map[string]yaml.Node `yaml:"patterns"`
	} `yaml:"rules"`
}

type toolLiteralRuleConstraint struct {
	Metavariable string `yaml:"metavariable"`
	Regex        string `yaml:"regex"`
}

func TestToolLiteralRule_CoversRegisteredTools(t *testing.T) {
	literalRegex := toolLiteralRuleRegex(t)
	reg := realRegistry(t, true, nil)
	ids := reg.IDs()
	for _, owner := range append(mcpstore.NewTools(nil, 0), budget.NewTool(nil, 0, false)) {
		ids = append(ids, owner.ID())
	}
	coreForever := []string{
		"sleep", "schedule", "task", "send_to_subagent", "get_subagent_result",
		"batch", "cancel_process", "config_edit", "config_read",
	}

	checked := make([]string, 0, len(ids))
	for _, id := range ids {
		if slices.Contains(coreForever, id) || strings.HasPrefix(id, "mcp__") {
			continue
		}

		checked = append(checked, id)
		assert.True(t, literalRegex.MatchString(strconv.Quote(id)),
			"tool ID %q is missing from the literal rule; add it to %s", id, toolLiteralRulePath)
	}
	require.NotEmpty(t, checked)
	assert.Contains(t, checked, "read")
	assert.Contains(t, checked, "set_budget")
}

func toolLiteralRuleRegex(t *testing.T) *regexp.Regexp {
	t.Helper()
	data, err := os.ReadFile(toolLiteralRulePath)
	require.NoError(t, err)

	var config toolLiteralRuleConfig
	require.NoError(t, yaml.Unmarshal(data, &config))

	var expression string
	for _, rule := range config.Rules {
		if rule.ID != "coagent-tool-literals-via-declarations" {
			continue
		}
		for _, pattern := range rule.Patterns {
			node, ok := pattern["metavariable-regex"]
			if !ok {
				continue
			}
			var constraint toolLiteralRuleConstraint
			require.NoError(t, node.Decode(&constraint))
			if constraint.Metavariable == "$LITERAL" {
				expression = constraint.Regex
			}
		}
	}
	require.NotEmpty(
		t,
		expression,
		"missing $LITERAL regex for coagent-tool-literals-via-declarations in %s",
		toolLiteralRulePath,
	)
	literalRegex, err := regexp.Compile(expression)
	require.NoError(t, err)
	return literalRegex
}
