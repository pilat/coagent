package mcp

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestNormalizeToolSchema(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "zod",
			raw: `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",
				"properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`,
			want: `{"type":"object","properties":{"url":{"type":"string"}},
				"required":["url"],"additionalProperties":false}`,
		},
		{
			name: "fastmcp",
			raw: `{"title":"add_numbersArguments","type":"object",
				"properties":{"a":{"type":"number"}},"required":["a"]}`,
			want: `{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}`,
		},
		{
			name: "top-level composition and metadata",
			raw: `{"type":"object","properties":{"id":{"type":"string"},"url":{"type":"string"}},
				"anyOf":[{"required":["id"]},{"required":["url"]}],"oneOf":[{"required":["id"]}],
				"description":"lookup","x-extension":true}`,
			want: `{"type":"object","properties":{"id":{"type":"string"},"url":{"type":"string"}}}`,
		},
		{
			name: "nullable object",
			raw:  `{"type":["object","null"],"properties":{},"required":[]}`,
			want: `{"type":"object","properties":{},"required":[]}`,
		},
		{
			name: "object preferred after another concrete type",
			raw:  `{"type":["null","string","object"]}`,
			want: `{"type":"object"}`,
		},
		{
			name: "first non-null type without object",
			raw:  `{"type":["null","string","number"]}`,
			want: `{"type":"string"}`,
		},
		{
			name: "parameterless object",
			raw:  `{"type":"object"}`,
			want: `{"type":"object"}`,
		},
		{
			name: "empty schema",
			raw:  `{}`,
			want: `{"type":"object"}`,
		},
		{
			name: "metadata only",
			raw:  `{"$schema":"http://json-schema.org/draft-07/schema#","title":"Arguments"}`,
			want: `{"type":"object"}`,
		},
		{
			name: "nested schemas remain intact",
			raw: `{"type":"object","properties":{"value":{"title":"Value","type":["string","null"],
				"anyOf":[{"$ref":"#/$defs/item"},{"type":"null"}]}},
				"additionalProperties":{"type":["number","null"]},
				"$defs":{"item":{"title":"Item","type":"string","description":"nested"}}}`,
			want: `{"type":"object","properties":{"value":{"title":"Value","type":["string","null"],
				"anyOf":[{"$ref":"#/$defs/item"},{"type":"null"}]}},
				"additionalProperties":{"type":["number","null"]},
				"$defs":{"item":{"title":"Item","type":"string","description":"nested"}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeToolSchema([]byte(tc.raw))
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(got))
		})
	}
}

func TestNormalizeToolSchemaInvalidJSON(t *testing.T) {
	for _, raw := range []string{`{`, `[]`, `"object"`} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeToolSchema([]byte(raw))
			require.ErrorContains(t, err, "decode tool schema")
			assert.Nil(t, got)
		})
	}
}

func TestClientToolSchema(t *testing.T) {
	t.Parallel()
	client := &Client{tools: map[string]mcp.Tool{
		"lookup": {
			Name: "lookup",
			InputSchema: mcp.ToolInputSchema{
				Type:                 "object",
				Properties:           map[string]any{"id": map[string]any{"$ref": "#/$defs/id"}},
				Required:             []string{"id"},
				AdditionalProperties: false,
				Defs:                 map[string]any{"id": map[string]any{"type": "string"}},
			},
		},
	}}
	got, err := client.ToolSchema("lookup")
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"object","properties":{"id":{"$ref":"#/$defs/id"}},
		"required":["id"],"additionalProperties":false,"$defs":{"id":{"type":"string"}}}`, string(got))
}

func TestClientToolSchemaFromToolsList(t *testing.T) {
	t.Parallel()
	var listed mcp.ListToolsResult
	require.NoError(t, json.Unmarshal([]byte(`{"tools":[{
		"name":"lookup",
		"inputSchema":{
			"$schema":"http://json-schema.org/draft-07/schema#",
			"title":"LookupArguments","type":"object",
			"properties":{"id":{"type":"string"}},"required":["id"],
			"anyOf":[{"required":["id"]}]
		}
	}]}`), &listed))
	require.Len(t, listed.Tools, 1)
	client := &Client{tools: map[string]mcp.Tool{listed.Tools[0].Name: listed.Tools[0]}}
	got, err := client.ToolSchema("lookup")
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`, string(got))
}

func TestClientToolSchemaEmptyInputSchema(t *testing.T) {
	t.Parallel()
	client := &Client{tools: map[string]mcp.Tool{"empty": {}}}
	got, err := client.ToolSchema("empty")
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"object","properties":{},"required":[]}`, string(got))
}

func TestMCPToolParametersNormalizedSchema(t *testing.T) {
	for _, serverName := range []string{"other", tool.PlaywrightServerName} {
		t.Run(serverName, func(t *testing.T) {
			t.Parallel()
			normalized, err := normalizeToolSchema([]byte(`{
				"$schema":"http://json-schema.org/draft-07/schema#","title":"LookupArguments",
				"type":["object","null"],"properties":{"id":{"type":"string"}},
				"required":["id"],"additionalProperties":false
			}`))
			require.NoError(t, err)
			mcpTool := newMCPTool(serverName, ToolMeta{Name: "lookup", Schema: normalized}, nil, nil)
			var got map[string]any
			require.NoError(t, json.Unmarshal(mcpTool.Parameters(), &got))
			assert.NotContains(t, got, "$schema")

			if serverName == tool.PlaywrightServerName {
				properties := got["properties"].(map[string]any)
				state := properties[tool.BrowserTaskState].(map[string]any)
				assert.Equal(t, "string", state["type"])
				assert.NotEmpty(t, state["description"])
				assert.Equal(t, []any{"id", tool.BrowserTaskState}, got["required"])
				delete(properties, tool.BrowserTaskState)
				got["required"] = []any{"id"}
			}

			var want map[string]any
			require.NoError(t, json.Unmarshal(normalized, &want))
			assert.Equal(t, want, got)
			assert.JSONEq(t, `{"type":"object","properties":{"id":{"type":"string"}},
				"required":["id"],"additionalProperties":false}`, string(mcpTool.meta.Schema))
		})
	}
}
