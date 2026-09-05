package session

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

// fakeRepoStateClient is a controlled git.Client for delta tests: only
// RepositoryState is meaningful, the rest refuse to run.
type fakeRepoStateClient struct {
	mu    sync.Mutex
	state git.RepositoryState
	err   error
	calls int
}

func (f *fakeRepoStateClient) RepositoryState(_ context.Context, _ string) (git.RepositoryState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	return f.state, f.err
}

func (f *fakeRepoStateClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func (f *fakeRepoStateClient) Clone(_ context.Context, _, _ string) error {
	return errors.New("fakeRepoStateClient: Clone must not be called")
}

func (f *fakeRepoStateClient) Pull(_ context.Context, _ string) error {
	return errors.New("fakeRepoStateClient: Pull must not be called")
}

func (f *fakeRepoStateClient) IsCloned(_ context.Context, _ string) bool { return false }

func (f *fakeRepoStateClient) GetRemoteURL(_ context.Context, _ string) (string, error) {
	return "", errors.New("fakeRepoStateClient: GetRemoteURL must not be called")
}

func (f *fakeRepoStateClient) HealthCheck(_ context.Context, _ string) error {
	return errors.New("fakeRepoStateClient: HealthCheck must not be called")
}

var _ git.Client = (*fakeRepoStateClient)(nil)

// newDeltaSession builds a build-agent session over workDir with the given git
// client; the caller keeps its own reference to the LLM recorder it passes in.
func newDeltaSession(t *testing.T, workDir string, gc git.Client, llmClient llm.Client) *svc {
	t.Helper()

	reg := tool.NewRegistry()
	for _, id := range []string{"read", "bash", tool.IDSkill} {
		reg.Register(testTool{id: id})
	}

	p := params{
		Config:    &config.Config{WorkDir: workDir, Model: "test-model"},
		LLMClient: llmClient,
		TodoStore: todo.New(),
		Loader:    loader.New(),
		Registry:  reg,
		GitClient: gc,
	}

	sess, err := newWithOptions(context.Background(), p, options{ID: 1, AgentType: registry.AgentTypeBuild})
	require.NoError(t, err)

	return sess.(*svc)
}

// gitDelta extracts the last <git-state> block from a transcript message.
func gitDelta(content string) (string, bool) {
	start := strings.LastIndex(content, gitStateMarker)
	if start < 0 {
		return "", false
	}

	end := strings.LastIndex(content[:start], gitStateMarker)
	if end < 0 {
		return "", false
	}

	report := content[end+len(gitStateMarker) : start]

	return strings.TrimPrefix(strings.TrimSuffix(report, "\n"), "\n"), true
}

func TestAppendGitStateDelta_FirstInputSendsFullSnapshot(t *testing.T) {
	s := newDeltaSession(t, t.TempDir(), &fakeRepoStateClient{
		state: git.RepositoryState{
			Status: git.RepositoryAvailable, Branch: "main", Hash: "abcdef123456",
			Staged: 1, Unstaged: 2, Untracked: 1,
		},
	}, &promptRecordingLLM{})

	out := s.appendGitStateDelta(context.Background(), "user text")

	report, ok := gitDelta(out)
	require.True(t, ok, "first input must carry the delta")
	assert.Equal(t, "Git repository: yes\nBranch: \"main\"\nHEAD: abcdef123456\n"+
		"Working tree: dirty (staged: 1, unstaged: 2, untracked: 1, conflicted: 0)", report)
	assert.True(t, strings.HasPrefix(out, "user text\n\n<git-state>"), "delta rides after the user text")
}

func TestAppendGitStateDelta_UnchangedStateIsSuppressed(t *testing.T) {
	state := git.RepositoryState{Status: git.RepositoryAvailable, Branch: "main", Hash: "abcdef123456"}
	gc := &fakeRepoStateClient{state: state}
	s := newDeltaSession(t, t.TempDir(), gc, &promptRecordingLLM{})

	require.NoError(t, s.ms.addUserMessage(context.Background(), "first\n\n<git-state>\n"+
		renderGitState(state)+"\n<git-state>"))

	out := s.appendGitStateDelta(context.Background(), "second input")

	_, ok := gitDelta(out)
	assert.False(t, ok, "an unchanged state must not re-send the delta")
	assert.Equal(t, "second input", out)
	assert.Equal(t, 1, gc.callCount(), "only the delta-ingestion probe ran; no turn-time probes")
}

func TestAppendGitStateDelta_ChangedStateIsSent(t *testing.T) {
	gc := &fakeRepoStateClient{
		state: git.RepositoryState{Status: git.RepositoryAvailable, Branch: "main", Hash: "abcdef123456"},
	}
	s := newDeltaSession(t, t.TempDir(), gc, &promptRecordingLLM{})

	require.NoError(t, s.ms.addUserMessage(context.Background(), "first\n\n<git-state>\n"+
		renderGitState(gc.state)+"\n<git-state>"))

	gc.mu.Lock()
	gc.state = git.RepositoryState{
		Status: git.RepositoryAvailable, Branch: "feature", Hash: "abcdef123456", Untracked: 3,
	}
	gc.mu.Unlock()

	out := s.appendGitStateDelta(context.Background(), "next input")

	report, ok := gitDelta(out)
	require.True(t, ok, "a changed state must re-send the delta")
	assert.Contains(t, report, `Branch: "feature"`)
	assert.Contains(t, report, "untracked: 3")
}

func TestAppendGitStateDelta_MissingGitClientLeavesInputUntouched(t *testing.T) {
	s := newDeltaSession(t, t.TempDir(), nil, &promptRecordingLLM{})

	out := s.appendGitStateDelta(context.Background(), "user text")

	assert.Equal(t, "user text", out)
}

func TestAppendGitStateDelta_FailedProbeSendsUnavailableOnce(t *testing.T) {
	gc := &fakeRepoStateClient{err: errors.New("git status probe: exit status 128")}
	s := newDeltaSession(t, t.TempDir(), gc, &promptRecordingLLM{})

	out := s.appendGitStateDelta(context.Background(), "user text")

	report, ok := gitDelta(out)
	require.True(t, ok)
	assert.Equal(t, "Git state: unavailable", report)
	assert.NotContains(t, out, "exit status 128", "probe error text must not reach the model")
}

func TestAppendGitStateDelta_BranchIsQuoted(t *testing.T) {
	// The collector truncates to 128 runes before rendering; the renderer's
	// job is safe quoting of whatever it receives, control chars included.
	raw := "feature\nbad\x1b[31m"
	s := newDeltaSession(t, t.TempDir(), &fakeRepoStateClient{
		state: git.RepositoryState{
			Status: git.RepositoryAvailable,
			Branch: raw,
			Hash:   "abcdef123456",
		},
	}, &promptRecordingLLM{})

	out := s.appendGitStateDelta(context.Background(), "user text")

	assert.Contains(t, out, "Branch: "+strconv.Quote(raw))
	assert.NotContains(t, out, "\x1b[", "raw control characters must stay quoted")
}

func TestLastGitState_ScansFromTail(t *testing.T) {
	old := "Git repository: yes\nBranch: \"old\"\nHEAD: aaaaaaaaaaaa\nWorking tree: clean"
	updated := "Git repository: yes\nBranch: \"new\"\nHEAD: bbbbbbbbbbbb\nWorking tree: clean"

	messages := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "first\n\n<git-state>\n" + old + "\n<git-state>"},
		{Role: llmwire.RoleAssistant, Content: "hi"},
		{Role: llmwire.RoleUser, Content: "second\n\n<git-state>\n" + updated + "\n<git-state>"},
	}

	got, ok := gitDelta(messages[len(messages)-1].Content)
	require.True(t, ok)
	assert.Equal(t, updated, got)
	assert.Equal(t, updated, lastGitState(messages))
}

func TestLastGitState_NoneOrMalformedSendsAgain(t *testing.T) {
	assert.Empty(t, lastGitState(nil))

	// A marker without its closing pair must not crash or return data.
	malformed := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "broken\n\n<git-state>\nno closing marker"},
	}
	assert.Empty(t, lastGitState(malformed))
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
