package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestComputeWorktreePaths(t *testing.T) {
	now := time.Date(2026, 3, 19, 19, 30, 55, 0, time.UTC)

	tests := []struct {
		name         string
		originalPath string
		gitRoot      string
		wantWT       string
		wantWorkDir  string
		wantBranch   string
	}{
		{
			name:         "repo root selected",
			originalPath: "/home/user/monorepo",
			gitRoot:      "/home/user/monorepo",
			wantWT:       "/home/user/monorepo-20260319-193055",
			wantWorkDir:  "/home/user/monorepo-20260319-193055",
			wantBranch:   "monorepo-20260319-193055",
		},
		{
			name:         "subfolder selected",
			originalPath: "/home/user/monorepo/services/api",
			gitRoot:      "/home/user/monorepo",
			wantWT:       "/home/user/monorepo-20260319-193055",
			wantWorkDir:  "/home/user/monorepo-20260319-193055/services/api",
			wantBranch:   "monorepo-20260319-193055",
		},
		{
			name:         "trailing slash on original path",
			originalPath: "/home/user/monorepo/",
			gitRoot:      "/home/user/monorepo",
			wantWT:       "/home/user/monorepo-20260319-193055",
			wantWorkDir:  "/home/user/monorepo-20260319-193055",
			wantBranch:   "monorepo-20260319-193055",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wt, wd, branch := ComputeWorktreePaths(tt.originalPath, tt.gitRoot, now)
			assert.Equal(t, tt.wantWT, wt, "worktreePath")
			assert.Equal(t, tt.wantWorkDir, wd, "fullWorkDir")
			assert.Equal(t, tt.wantBranch, branch, "branchName")
		})
	}
}
