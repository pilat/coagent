package sessionprompt

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

const (
	gitStateOpenMarker  = "<git-state>"
	gitStateCloseMarker = "</git-state>"

	noGitStateReport = "Git state: unavailable"

	// Bound unexpected probe output before it reaches the model.
	maxProbeErrorRunes = 255
)

// CurrentGitState degrades probe failures to a redacted report so ingestion can continue.
func (s *Builder) CurrentGitState(ctx context.Context) string {
	if s.GitClient == nil {
		return noGitStateReport
	}

	state, err := s.GitClient.RepositoryState(ctx, s.WorkDir)
	if err != nil {
		// The contract ties a non-nil error to Unavailable; never trust a
		// partially-populated state alongside it.
		return noGitStateReport + ": " + boundProbeError(logger.Redact(err.Error()))
	}

	return renderGitState(state)
}

// AppendGitStateDelta injects changed state; malformed or missing prior reports force a refresh.
// Without a Git client, content passes through unchanged.
func (s *Builder) AppendGitStateDelta(ctx context.Context, content string, messages []llmwire.Message) string {
	if s.GitClient == nil {
		return content
	}

	state := s.CurrentGitState(ctx)

	last := lastGitState(messages)
	if last != "" && last == state {
		return content
	}

	return content + "\n\n" + gitStateOpenMarker + "\n" + state + "\n" + gitStateCloseMarker
}

func boundProbeError(s string) string {
	runes := []rune(s)
	if len(runes) <= maxProbeErrorRunes {
		return s
	}

	return string(runes[:maxProbeErrorRunes]) + "…"
}

// Quote branch text because repository metadata is data rather than instructions.
func renderGitState(state git.RepositoryState) string {
	switch state.Status {
	case git.RepositoryAvailable:
	case git.RepositoryNotRepository:
		return "Git repository: no"
	case git.RepositoryUnavailable:
		return noGitStateReport
	}

	branch := strconv.Quote(state.Branch)
	if state.Branch == git.DetachedHeadMarker {
		branch = git.DetachedHeadMarker
	}

	head := state.Hash
	if head == "" {
		head = "none (no commits yet)"
	}

	workingTree := "clean"
	if state.Staged != 0 || state.Unstaged != 0 || state.Untracked != 0 || state.Conflicted != 0 {
		workingTree = "dirty (staged: " + strconv.Itoa(state.Staged) +
			", unstaged: " + strconv.Itoa(state.Unstaged) +
			", untracked: " + strconv.Itoa(state.Untracked) +
			", conflicted: " + strconv.Itoa(state.Conflicted) + ")"
	}

	return "Git repository: yes\nBranch: " + branch + "\nHEAD: " + head +
		"\nWorking tree: " + workingTree
}

// A malformed envelope forces a fresh report rather than preserving stale Git facts.
func lastGitState(messages []llmwire.Message) string {
	for _, m := range slices.Backward(messages) {
		if m.Role != llmwire.RoleUser {
			continue
		}

		if !strings.Contains(m.Content, gitStateOpenMarker) &&
			!strings.Contains(m.Content, gitStateCloseMarker) {
			continue
		}

		report, found := extractGitState(m.Content)
		if found {
			return report
		}

		return ""
	}

	return ""
}

func extractGitState(content string) (string, bool) {
	end := strings.LastIndex(content, gitStateCloseMarker)
	if end >= 0 {
		start := strings.LastIndex(content[:end], gitStateOpenMarker)
		if start < 0 {
			return "", false
		}

		return trimGitState(content[start+len(gitStateOpenMarker) : end]), true
	}

	// Older transcript rows used a second opening marker as the closer.
	end = strings.LastIndex(content, gitStateOpenMarker)
	if end < 0 {
		return "", false
	}

	start := strings.LastIndex(content[:end], gitStateOpenMarker)
	if start < 0 {
		return "", false
	}

	return trimGitState(content[start+len(gitStateOpenMarker) : end]), true
}

func trimGitState(report string) string {
	return strings.TrimPrefix(strings.TrimSuffix(report, "\n"), "\n")
}
