package humanize

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0B"},
		{500, "500B"},
		{1023, "1023B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1024*1024 - 1, "1024.0KB"},
		{1048576, "1.0MB"},
		{1572864, "1.5MB"},
		{1024*1024*1024 - 1, "1024.0MB"},
		{1073741824, "1.0GB"},
		{1610612736, "1.5GB"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, FormatSize(tt.bytes))
	}
}
