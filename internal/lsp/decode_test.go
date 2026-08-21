package lsp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeLocations(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{name: "null", wire: "null"},
		{
			name: "one location",
			wire: `{"uri":"file:///one.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}`,
			want: "file:///one.go",
		},
		{
			name: "location link falls back to target range",
			wire: `[{
				"targetUri":"file:///fallback.go",
				"targetRange":{"start":{"line":2,"character":0},"end":{"line":2,"character":1}}
			}]`,
			want: "file:///fallback.go",
		},
		{
			name: "location links",
			wire: `[{"targetUri":"file:///two.go","targetRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"targetSelectionRange":{"start":{"line":1,"character":0},"end":{"line":1,"character":1}}}]`,
			want: "file:///two.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			locations, err := decodeLocations(json.RawMessage(tt.wire))
			require.NoError(t, err)
			if tt.want == "" {
				assert.Empty(t, locations)
				return
			}

			require.Len(t, locations, 1)
			assert.Equal(t, tt.want, locations[0].URI)
		})
	}
}

func TestDecodeDocumentSymbolsAndHoverUnions(t *testing.T) {
	symbols, err := decodeDocumentSymbols(
		json.RawMessage(
			`[{"name":"Flat","kind":12,"location":{"uri":"file:///a.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}}]`,
		),
	)
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "Flat", symbols[0].Name)

	hover, err := decodeHover(json.RawMessage(`{"contents":["alpha",{"language":"go","value":"beta"}]}`))
	require.NoError(t, err)
	require.NotNil(t, hover)
	assert.Equal(t, "alpha\n\n```go\nbeta\n```", hover.Contents.Value)

	hover, err = decodeHover(json.RawMessage("null"))
	require.NoError(t, err)
	assert.Nil(t, hover)
}

func TestDecodeHoverPlaintextMarkedStringArray(t *testing.T) {
	hover, err := decodeHover(json.RawMessage(`{"contents":["alpha","beta"]}`))
	require.NoError(t, err)
	require.NotNil(t, hover)
	assert.Equal(t, "plaintext", hover.Contents.Kind)
	assert.Equal(t, "alpha\n\nbeta", hover.Contents.Value)
}

func TestDecodeHoverAcceptsEmptyMarkupContent(t *testing.T) {
	hover, err := decodeHover(json.RawMessage(`{"contents":{"kind":"markdown","value":""}}`))
	require.NoError(t, err)
	require.NotNil(t, hover)
	assert.Equal(t, MarkupContent{Kind: "markdown"}, hover.Contents)
}

func TestDecodeHoverRejectsMarkupContentWithoutValue(t *testing.T) {
	_, err := decodeHover(json.RawMessage(`{"contents":{"kind":"markdown"}}`))
	require.Error(t, err)
}

func TestDecodeFlatDocumentSymbolPreservesContainerName(t *testing.T) {
	symbols, err := decodeDocumentSymbols(json.RawMessage(`[
		{"name":"Method","kind":6,"containerName":"Receiver","location":{"uri":"file:///a.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}}
	]`))
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "Receiver", symbols[0].ContainerName)
}

func TestDiagnosticCodeAcceptsStringAndInteger(t *testing.T) {
	tests := []string{`{"code":"unknown"}`, `{"code":42}`}
	for _, wire := range tests {
		var diagnostic Diagnostic
		require.NoError(t, json.Unmarshal([]byte(wire), &diagnostic))
		assert.NotEmpty(t, diagnostic.Code)
		encoded, err := json.Marshal(diagnostic.Code)
		require.NoError(t, err)
		var expected struct {
			Code json.RawMessage `json:"code"`
		}
		require.NoError(t, json.Unmarshal([]byte(wire), &expected))
		assert.JSONEq(t, string(expected.Code), string(encoded))
	}

	var diagnostic Diagnostic
	require.Error(t, json.Unmarshal([]byte(`{"code":null}`), &diagnostic))
	require.Error(t, json.Unmarshal([]byte(`{"code":1.5}`), &diagnostic))
}

func TestDecodeWorkspaceSymbolsPreservesURIOnlyLocationAndMetadata(t *testing.T) {
	symbols, err := decodeWorkspaceSymbols(json.RawMessage(`[
		{"name":"Old","kind":12,"tags":[1],"deprecated":true,"containerName":"pkg","location":{"uri":"file:///a.go"}}
	]`))
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Nil(t, symbols[0].Location.Range)
	assert.Equal(t, []int{1}, symbols[0].Tags)
	assert.True(t, symbols[0].Deprecated)
	assert.Equal(t, "pkg", symbols[0].ContainerName)
}
