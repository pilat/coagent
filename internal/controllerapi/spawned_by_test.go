package controllerapi

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSpawnedBy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string]any
		want  int64
	}{
		{name: "absent"},
		{name: "JSON number", attrs: map[string]any{"spawned_by": float64(12)}, want: 12},
		{name: "integer", attrs: map[string]any{"spawned_by": int64(12)}},
		{name: "string", attrs: map[string]any{"spawned_by": "12"}},
		{name: "boolean", attrs: map[string]any{"spawned_by": true}},
		{name: "nil", attrs: map[string]any{"spawned_by": nil}},
		{name: "json.Number", attrs: map[string]any{"spawned_by": json.Number("12")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, SpawnedBy(tc.attrs))
		})
	}
}

func TestSpawnedByApp(t *testing.T) {
	for _, value := range []any{nil, float64(12), int64(12), true, json.Number("12"), "bot"} {
		want := ""
		if value == "bot" {
			want = "bot"
		}
		assert.Equal(t, want, SpawnedByApp(map[string]any{SessionAttributeSpawnedByApp: value}))
	}
	assert.Empty(t, SpawnedByApp(nil))
}
