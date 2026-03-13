package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSortEfforts(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"descending input is reordered", []string{"max", "high", "low"}, []string{"low", "high", "max"}},
		{"ascending input survives", []string{"low", "medium", "high"}, []string{"low", "medium", "high"}},
		{"unknown levels are dropped", []string{"high", "turbo", ""}, []string{"high"}},
		{"duplicates collapse", []string{"high", "high", "low"}, []string{"low", "high"}},
		{"all unknown yields nil", []string{"turbo", "ludicrous"}, nil},
		{"empty yields nil", nil, nil},
		{
			"full vocabulary keeps canonical order",
			[]string{"max", "xhigh", "high", "medium", "low", "minimal", "none"},
			[]string{"none", "minimal", "low", "medium", "high", "xhigh", "max"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SortEfforts(tt.in))
		})
	}
}

func TestClampEffort(t *testing.T) {
	tests := []struct {
		name    string
		level   string
		allowed []string
		want    string
	}{
		{"exact match passes through", "high", []string{"low", "high"}, "high"},
		{"empty allowlist passes through", "medium", nil, "medium"},
		{"medium clamps up to the only near level", "medium", []string{"high", "xhigh"}, "high"},
		{"medium clamps down when below is nearer", "medium", []string{"none", "low"}, "low"},
		{"a tie resolves to the weaker level", "medium", []string{"low", "high"}, "low"},
		{"max clamps to the strongest offered", "max", []string{"low", "medium"}, "medium"},
		{"unknown level is left alone", "turbo", []string{"low", "high"}, "turbo"},
		{"allowlist of unknowns is left alone", "medium", []string{"turbo"}, "medium"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ClampEffort(tt.level, tt.allowed))
		})
	}
}
