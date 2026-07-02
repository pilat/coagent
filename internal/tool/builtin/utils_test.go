package builtin

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/coagenthome"
)

func TestResolvePath(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()

	tests := []struct {
		name    string
		workDir string
		path    string
		want    string
	}{
		{
			name:    "relative path",
			workDir: "/project",
			path:    "src/main.go",
			want:    "/project/src/main.go",
		},
		{
			name:    "absolute path unchanged",
			workDir: "/project",
			path:    "/etc/hosts",
			want:    "/etc/hosts",
		},
		{
			name:    "absolute path cleaned",
			workDir: "/project",
			path:    "/etc/../etc/hosts",
			want:    "/etc/hosts",
		},
		{
			name:    "tilde expands to home",
			workDir: "/project",
			path:    "~/config.yaml",
			want:    filepath.Join(home, "config.yaml"),
		},
		{
			name:    "tilde nested path",
			workDir: "/project",
			path:    "~/.coagent/config.yaml",
			want:    filepath.Join(home, ".coagent/config.yaml"),
		},
		{
			name:    "tilde alone",
			workDir: "/project",
			path:    "~",
			want:    home,
		},
		{
			name:    "dot resolves to workDir",
			workDir: "/project",
			path:    ".",
			want:    "/project",
		},
		{
			name:    "parent traversal works",
			workDir: "/project/sub",
			path:    "../other/file.go",
			want:    "/project/other/file.go",
		},
		{
			name:    "bare filename",
			workDir: "/project",
			path:    "file.txt",
			want:    "/project/file.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePath(tt.workDir, tt.path)
			assert.Equal(t, tt.want, got)
		})
	}
}
