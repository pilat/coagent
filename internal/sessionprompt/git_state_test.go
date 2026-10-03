package sessionprompt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llmwire"
)

func gitDelta(content string) (string, bool) { return extractGitState(content) }
func TestLastGitState_ScansFromTail(t *testing.T) {
	old := "Git repository: yes\nBranch: \"old\"\nHEAD: aaaaaaaaaaaa\nWorking tree: clean"
	updated := "Git repository: yes\nBranch: \"new\"\nHEAD: bbbbbbbbbbbb\nWorking tree: clean"

	messages := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "first\n\n<git-state>\n" + old + "\n</git-state>"},
		{Role: llmwire.RoleAssistant, Content: "hi"},
		{Role: llmwire.RoleUser, Content: "second\n\n<git-state>\n" + updated + "\n</git-state>"},
	}

	got, ok := gitDelta(messages[len(messages)-1].Content)
	require.True(t, ok)
	assert.Equal(t, updated, got)
	assert.Equal(t, updated, lastGitState(messages))
}

func TestLastGitState_ReadsLegacySymmetricEnvelope(t *testing.T) {
	report := "Git repository: yes\nBranch: \"main\"\nHEAD: aaaaaaaaaaaa\nWorking tree: clean"
	messages := []llmwire.Message{{
		Role: llmwire.RoleUser, Content: "first\n\n<git-state>\n" + report + "\n<git-state>",
	}}

	assert.Equal(t, report, lastGitState(messages))
}

func TestLastGitState_NoneOrMalformedSendsAgain(t *testing.T) {
	assert.Empty(t, lastGitState(nil))

	// A marker without its closing pair must not crash or return data.
	malformed := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "broken\n\n<git-state>\nno closing marker"},
	}
	assert.Empty(t, lastGitState(malformed))

	malformed = append([]llmwire.Message{{
		Role: llmwire.RoleUser, Content: "old\n\n<git-state>\nold state\n</git-state>",
	}}, malformed...)
	assert.Empty(t, lastGitState(malformed), "a newer malformed envelope must not expose stale state")
}

func TestRenderGitState_NotRepositoryAndUnborn(t *testing.T) {
	assert.Equal(t, "Git repository: no",
		renderGitState(git.RepositoryState{Status: git.RepositoryNotRepository}))
	assert.Equal(t, "Git state: unavailable",
		renderGitState(git.RepositoryState{Status: git.RepositoryUnavailable}))

	unborn := renderGitState(git.RepositoryState{
		Status: git.RepositoryAvailable, Branch: "main", Hash: "",
	})
	assert.Contains(t, unborn, "HEAD: none (no commits yet)")
	assert.Contains(t, unborn, "Working tree: clean")
}

type stateGitClient struct {
	git.Client
	state git.RepositoryState
	err   error
	calls int
}

func (c *stateGitClient) RepositoryState(context.Context, string) (git.RepositoryState, error) {
	c.calls++
	return c.state, c.err
}

func TestAppendGitStateDelta(t *testing.T) {
	initial := git.RepositoryState{
		Status:    git.RepositoryAvailable,
		Branch:    "main",
		Hash:      "abcdef",
		Staged:    1,
		Untracked: 2,
	}
	for _, tc := range []struct {
		name      string
		client    bool
		state     git.RepositoryState
		previous  string
		err       error
		want      string
		unchanged bool
	}{
		{name: "first input sends snapshot", client: true, state: initial, want: "dirty (staged: 1, unstaged: 0, untracked: 2, conflicted: 0)"},
		{name: "unchanged state is suppressed", client: true, state: initial, previous: renderGitState(initial), unchanged: true},
		{name: "changed state is sent", client: true, state: git.RepositoryState{Status: git.RepositoryAvailable, Branch: "feature", Hash: "abcdef"}, previous: renderGitState(initial), want: `Branch: "feature"`},
		{name: "missing client leaves input untouched", unchanged: true},
		{name: "probe error sends unavailable", client: true, err: errors.New("probe timed out"), want: "Git state: unavailable: probe timed out"},
		{name: "probe reason bounded", client: true, err: errors.New(strings.Repeat("x", 500)), want: "…"},
		{name: "branch safely quoted", client: true, state: git.RepositoryState{Status: git.RepositoryAvailable, Branch: "feature\nbad\x1b[31m"}, want: `Branch: "feature\nbad\x1b[31m"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := NewBuilder("", "")
			client := &stateGitClient{state: tc.state, err: tc.err}
			if tc.client {
				prompt.GitClient = client
			}
			var prior []llmwire.Message
			if tc.previous != "" {
				prior = []llmwire.Message{
					{Role: llmwire.RoleUser, Content: "first\n\n<git-state>\n" + tc.previous + "\n</git-state>"},
				}
			}
			out := prompt.AppendGitStateDelta(t.Context(), "user text", prior)
			if tc.unchanged {
				assert.Equal(t, "user text", out)
			} else {
				report, ok := extractGitState(out)
				require.True(t, ok)
				assert.Contains(t, report, tc.want)
				assert.LessOrEqual(t, len([]rune(report)), 280)
			}
			if tc.client {
				assert.Equal(t, 1, client.calls)
			}
		})
	}
}
