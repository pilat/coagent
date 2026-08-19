package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestResolveModelChoice(t *testing.T) {
	t.Parallel()

	models := []controllerapi.ConfigModelInfo{{ID: "model-a"}, {ID: "model-b"}}
	tests := []struct {
		name   string
		choice string
		want   string
		ok     bool
	}{
		{name: "number", choice: "2", want: "model-b", ok: true},
		{name: "model id", choice: "model-a", want: "model-a", ok: true},
		{name: "zero", choice: "0"},
		{name: "past end", choice: "3"},
		{name: "unknown id", choice: "model-c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := resolveModelChoice(models, tt.choice)

			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got.ID)
		})
	}
}

func TestResolveStringChoice(t *testing.T) {
	t.Parallel()

	values := []string{"low", "high"}

	assertChoice := func(t *testing.T, choice, want string, wantOK bool) {
		t.Helper()

		got, ok := resolveStringChoice(values, choice)
		assert.Equal(t, wantOK, ok)
		assert.Equal(t, want, got)
	}

	assertChoice(t, "1", "low", true)
	assertChoice(t, "high", "high", true)
	assertChoice(t, "ultra", "", false)
}

func TestModelNameFallbacks(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Display", modelName(controllerapi.ConfigModelInfo{
		ID: "id", Name: "Name", DisplayName: "Display",
	}))
	assert.Equal(t, "Name", modelName(controllerapi.ConfigModelInfo{ID: "id", Name: "Name"}))
	assert.Equal(t, "id", modelName(controllerapi.ConfigModelInfo{ID: "id"}))
}
