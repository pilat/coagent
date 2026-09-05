package session

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llmwire"
)

// gitStateMarker delimits an injected Git-state delta inside a user-role
// transcript row. lastGitState scans for this exact marker from the tail, so
// compaction (which preserves the verbatim raw tail) cannot lose it.
const (
	gitStateMarker = "<git-state>"

	// noGitStateReport is sent when the probe itself failed: the model must
	// know its Git awareness is degraded rather than assume freshness.
	noGitStateReport = "Git state: unavailable"
)

// currentGitState probes the session workDir. Any probe failure degrades to
// the unavailable report; it must never fail the ingestion path.
func (s *svc) currentGitState(ctx context.Context) string {
	if s.gitClient == nil {
		return noGitStateReport
	}

	state, err := s.gitClient.RepositoryState(ctx, s.workDir)
	if err != nil {
		// The contract ties a non-nil error to Unavailable; never trust a
		// partially-populated state alongside it.
		return noGitStateReport
	}

	return renderGitState(state)
}

// renderGitState formats the model-facing report. Branch text is data, not
// instructions: Go-quoted here, bounded to 128 runes by the collector.
func renderGitState(state git.RepositoryState) string {
	switch state.Status {
	case git.RepositoryAvailable:
		// Rendered below.
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

// appendGitStateDelta returns content with the current Git state appended when
// it differs from the last injected delta. A missing previous delta (fresh
// session, or anything the scan cannot resolve) always sends: a redundant
// ~30-token block costs less than the model acting on stale Git facts.
// Without a git client nothing is appended at all.
func (s *svc) appendGitStateDelta(ctx context.Context, content string) string {
	if s.gitClient == nil {
		return content
	}

	state := s.currentGitState(ctx)

	last := lastGitState(s.ms.getMessages())
	if last != "" && last == state {
		return content
	}

	return content + "\n\n" + gitStateMarker + "\n" + state + "\n" + gitStateMarker
}

// lastGitState returns the report inside the most recent gitStateMarker pair,
// or "" when none is present. Any malformed match returns "": the caller then
// sends a fresh delta, which is always the safe direction.
func lastGitState(messages []llmwire.Message) string {
	for _, m := range slices.Backward(messages) {
		if m.Role != llmwire.RoleUser {
			continue
		}

		start := strings.LastIndex(m.Content, gitStateMarker)
		if start < 0 {
			continue
		}

		end := strings.LastIndex(m.Content[:start], gitStateMarker)
		if end < 0 {
			return ""
		}

		report := m.Content[end+len(gitStateMarker) : start]

		return strings.TrimPrefix(strings.TrimSuffix(report, "\n"), "\n")
	}

	return ""
}
