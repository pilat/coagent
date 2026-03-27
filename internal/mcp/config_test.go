package mcp

import (
	"slices"
	"testing"
)

func TestServerConfig_IsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		config   ServerConfig
		expected bool
	}{
		{
			name:     "default enabled",
			config:   ServerConfig{},
			expected: true,
		},
		{
			name:     "explicitly enabled",
			config:   ServerConfig{Enabled: boolPtr(true)},
			expected: true,
		},
		{
			name:     "explicitly disabled via Enabled field",
			config:   ServerConfig{Enabled: boolPtr(false)},
			expected: false,
		},
		{
			name:     "disabled via Disabled field",
			config:   ServerConfig{Disabled: true},
			expected: false,
		},
		{
			name:     "Disabled field takes precedence over Enabled",
			config:   ServerConfig{Disabled: true, Enabled: boolPtr(true)},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.IsEnabled()
			if got != tt.expected {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestBuildEnv(t *testing.T) {
	t.Setenv("TEST_VAR", "test_value")

	tests := []struct {
		name     string
		envMap   map[string]string
		expected int
		contains string
	}{
		{
			name:     "nil map",
			envMap:   nil,
			expected: 0,
		},
		{
			name:     "empty map",
			envMap:   map[string]string{},
			expected: 0,
		},
		{
			name:     "simple vars",
			envMap:   map[string]string{"KEY": "value"},
			expected: 1,
			contains: "KEY=value",
		},
		{
			name:     "multiple vars",
			envMap:   map[string]string{"KEY1": "val1", "KEY2": "val2"},
			expected: 2,
		},
		{
			// Resolution belongs to the config loader; buildEnv must not reach
			// into the process environment on its own.
			name:     "value passes through verbatim",
			envMap:   map[string]string{"VERBATIM": "prefix_${TEST_VAR}_suffix"},
			expected: 1,
			contains: "VERBATIM=prefix_${TEST_VAR}_suffix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildEnv(tt.envMap)
			if len(result) != tt.expected {
				t.Errorf("buildEnv() returned %d items, expected %d", len(result), tt.expected)
			}
			if tt.contains != "" {
				found := slices.Contains(result, tt.contains)
				if !found && tt.expected > 0 {
					t.Errorf("buildEnv() result does not contain %q", tt.contains)
				}
			}
		})
	}
}

func boolPtr(b bool) *bool {
	return &b
}
