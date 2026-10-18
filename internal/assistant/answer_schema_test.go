package assistant

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type schemaNode struct {
	Type                 json.RawMessage   `json:"type"`
	Properties           json.RawMessage   `json:"properties"`
	Required             []string          `json:"required"`
	AdditionalProperties *bool             `json:"additionalProperties"` //nolint:tagliatelle // JSON Schema defines this spelling.
	Items                json.RawMessage   `json:"items"`
	AnyOf                []json.RawMessage `json:"anyOf"` //nolint:tagliatelle // JSON Schema defines this spelling.
}

func TestAssistantAnswerSchemaContract(t *testing.T) {
	t.Parallel()
	assertSchemaType(t, json.RawMessage(AnswerSchema), reflect.TypeFor[Answer]())
}

func TestAssistantAnswerWireRoundTrip(t *testing.T) {
	t.Parallel()
	full, err := Parse(`{"say":null,"do":[{"stop":{"session":1}}],"focus":null,"notes":[],"forget":[[1,2]]}`)
	require.NoError(t, err)
	omitted, err := Parse(`{"do":[{"stop":{"session":1}}],"forget":[[1,2]]}`)
	require.NoError(t, err)
	assert.Equal(t, omitted, full)
	nulls, err := Parse(`{"notes":null,"do":null,"forget":null}`)
	require.NoError(t, err)
	assert.Equal(t, Answer{}, nulls)

	want := Answer{
		Say: new("hello"), Focus: new("focus"),
		Do: []Action{
			{Open: &OpenAction{Project: "project", About: "purpose", Text: "go"}},
			{Send: &SendAction{Session: 1, Text: "continue"}},
			{Read: &ReadAction{Session: 1, Lines: 2}},
			{Stop: &StopAction{Session: 1}},
		},
		Notes:  map[string]*string{"user.lang": new("Russian"), "open.old": nil},
		Forget: [][]int64{{1, 2}},
	}
	data, err := json.Marshal(want)
	require.NoError(t, err)
	got, err := Parse(string(data))
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Contains(t, string(data), `"notes":[{"key":"open.old","text":null},{"key":"user.lang","text":"Russian"}]`)
}

func TestAssistantFilterUnchangedNotes(t *testing.T) {
	t.Parallel()
	desk := Desk{Notes: []Note{
		{Key: "user.lang", Text: "Russian", WrittenAt: 10},
		{Key: "open.old", Text: "old", WrittenAt: 10},
		{Key: "wait.new", Text: "retell", WrittenAt: 10},
		{Key: "project.new", Text: "purpose", WrittenAt: 10},
	}}
	a := Answer{
		Do: []Action{{Open: &OpenAction{Project: "new", About: "purpose", Text: "go"}}},
		Notes: map[string]*string{
			"user.lang": new("Russian"), "open.old": nil, "open.missing": nil,
			"wait.new": new("retell"), "project.new": new("purpose"), "open.changed": new("new"),
		},
	}
	FilterUnchangedNotes(&a, desk)
	assert.NotContains(t, a.Notes, "user.lang")
	assert.Contains(t, a.Notes, "open.old")
	assert.Contains(t, a.Notes, "open.missing")
	assert.Contains(t, a.Notes, "wait.new")
	assert.Contains(t, a.Notes, "project.new")
	assert.Contains(t, a.Notes, "open.changed")
}

func assertSchemaType(t *testing.T, raw json.RawMessage, typ reflect.Type) {
	t.Helper()
	var node schemaNode
	require.NoError(t, json.Unmarshal(raw, &node))
	if typ.Kind() == reflect.Pointer {
		if typ.Elem().Kind() == reflect.String {
			assert.JSONEq(t, `["string","null"]`, string(node.Type))
			return
		}
		typ = typ.Elem()
	}
	switch typ.Kind() { //nolint:exhaustive // Only contract field kinds are valid; all others must fail.
	case reflect.Struct:
		assertSchemaObject(t, node, typ)
	case reflect.Slice:
		assert.JSONEq(t, `"array"`, string(node.Type))
		assertSchemaType(t, node.Items, typ.Elem())
	case reflect.String:
		assert.JSONEq(t, `"string"`, string(node.Type))
	case reflect.Int, reflect.Int64:
		assert.JSONEq(t, `"integer"`, string(node.Type))
	default:
		t.Fatalf("unexpected contract type %s", typ)
	}
}

func assertSchemaObject(t *testing.T, node schemaNode, typ reflect.Type) {
	t.Helper()
	if typ == reflect.TypeFor[Action]() {
		require.Len(t, node.AnyOf, typ.NumField())
		for i, branch := range node.AnyOf {
			var object schemaNode
			require.NoError(t, json.Unmarshal(branch, &object))
			field := typ.Field(i)
			names, properties := schemaProperties(t, object)
			require.Equal(t, []string{jsonFieldName(field)}, names)
			assertSchemaType(t, properties[names[0]], field.Type)
		}
		return
	}
	names, properties := schemaProperties(t, node)
	var expected []string
	for field := range typ.Fields() {
		name := jsonFieldName(field)
		expected = append(expected, name)
		fieldType := field.Type
		if typ == reflect.TypeFor[Answer]() && name == "notes" {
			fieldType = reflect.TypeFor[[]noteEdit]()
		}
		assertSchemaType(t, properties[name], fieldType)
	}
	assert.Equal(t, expected, names)
}

func schemaProperties(t *testing.T, node schemaNode) ([]string, map[string]json.RawMessage) {
	t.Helper()
	assert.JSONEq(t, `"object"`, string(node.Type))
	require.NotNil(t, node.AdditionalProperties)
	assert.False(t, *node.AdditionalProperties)
	decoder := json.NewDecoder(strings.NewReader(string(node.Properties)))
	token, err := decoder.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), token)
	var names []string
	properties := map[string]json.RawMessage{}
	for decoder.More() {
		key, err := decoder.Token()
		require.NoError(t, err)
		name, ok := key.(string)
		require.True(t, ok)
		var value json.RawMessage
		require.NoError(t, decoder.Decode(&value))
		names = append(names, name)
		properties[name] = value
	}
	assert.Equal(t, names, node.Required)
	return names, properties
}

func jsonFieldName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	return name
}
