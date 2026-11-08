package builtin

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/tool"
)

func TestWebToolsDeclareUntrustedSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		candidate tool.Tool
		want      bool
	}{
		{candidate: newWebFetchTool(), want: true},
		{candidate: newWebSearchTool(&stubSearchProvider{}, 5), want: true},
		{candidate: &readTool{}},
		{candidate: &bashTool{}},
	}

	for _, tt := range tests {
		t.Run(tt.candidate.ID(), func(t *testing.T) {
			t.Parallel()

			_, declared := tt.candidate.(tool.UntrustedSource)
			assert.Equal(t, tt.want, declared)
		})
	}
}
