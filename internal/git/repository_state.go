package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// RepositoryStateStatus classifies the outcome of a repository-state probe.
type RepositoryStateStatus int

const (
	// RepositoryAvailable means workDir belongs to a Git work tree.
	RepositoryAvailable RepositoryStateStatus = iota
	// RepositoryNotRepository means workDir is not inside a Git repository.
	RepositoryNotRepository
	// RepositoryUnavailable means the probe failed or was inconclusive.
	RepositoryUnavailable
)

// DetachedHeadMarker is the model-facing branch label for a detached HEAD.
const DetachedHeadMarker = "detached HEAD"

const (
	maxBranchRunes = 128
	minHashLen     = 12
	maxHashLen     = 64

	// probeWaitDelay bounds the pipe-drain grace after a probe kill: the
	// 10s network drain is disproportionate for a five-second local probe.
	probeWaitDelay = time.Second
)

// repositoryProbeTimeout bounds every command of one state collection
// (var: tests shrink it).
var repositoryProbeTimeout = 5 * time.Second

// RepositoryState is a bounded, path-free snapshot of one work tree. It never
// carries file paths, commit subjects, remotes, or command output.
type RepositoryState struct {
	Status     RepositoryStateStatus
	Branch     string
	Hash       string
	Staged     int
	Unstaged   int
	Untracked  int
	Conflicted int
}

func (c *client) RepositoryState(ctx context.Context, workDir string) (RepositoryState, error) {
	ctx, cancel := context.WithTimeout(ctx, repositoryProbeTimeout)
	defer cancel()

	inside, err := isInsideWorkTree(ctx, workDir)
	if err != nil {
		return RepositoryState{Status: RepositoryUnavailable},
			fmt.Errorf("git repository probe: %w", err)
	}

	if !inside {
		return RepositoryState{Status: RepositoryNotRepository}, nil
	}

	state := RepositoryState{Status: RepositoryAvailable}

	branch, detached, err := probeBranch(ctx, workDir)
	if err != nil {
		return RepositoryState{Status: RepositoryUnavailable},
			fmt.Errorf("git branch probe: %w", err)
	}

	state.Branch = DetachedHeadMarker
	if !detached {
		state.Branch = truncateRunes(branch, maxBranchRunes)
	}

	hash, unborn, err := probeHash(ctx, workDir)
	if err != nil {
		return RepositoryState{Status: RepositoryUnavailable},
			fmt.Errorf("git HEAD probe: %w", err)
	}

	// An unborn branch (fresh init) has no resolvable HEAD; that is a missing
	// hash in an otherwise valid repository, not a failed probe.
	if !unborn {
		state.Hash = hash
	}

	counts, err := probeStatus(ctx, workDir)
	if err != nil {
		return RepositoryState{Status: RepositoryUnavailable},
			fmt.Errorf("git status probe: %w", err)
	}

	counts.applyTo(&state)

	return state, nil
}

// runGitProbe runs one read-only local git command. Stdout is the parsed
// input; stderr is classification-only and must never reach callers.
func runGitProbe(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = probeEnv()
	cmd.WaitDelay = probeWaitDelay

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	return stdout.String(), stderr.String(), err
}

// probeEnv disables prompting/locking and strips directory selectors: an
// inherited GIT_DIR/GIT_WORK_TREE would point the probes at another repository.
func probeEnv() []string {
	base := nonInteractiveGitEnv()
	env := make([]string, 0, len(base))

	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR":
			continue
		}

		env = append(env, kv)
	}

	return env
}

func isInsideWorkTree(ctx context.Context, workDir string) (bool, error) {
	stdout, stderr, err := runGitProbe(ctx, workDir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		// "not a git repository" is git's classifier for a non-repo
		// directory; any other failure stays inconclusive.
		if strings.Contains(stderr, "not a git repository") {
			return false, nil
		}

		return false, err
	}

	return strings.TrimSpace(stdout) == "true", nil
}

func probeBranch(ctx context.Context, dir string) (string, bool, error) {
	stdout, stderr, err := runGitProbe(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err == nil {
		return strings.TrimSpace(stdout), false, nil
	}

	// "not a symbolic ref" is git's detached-HEAD answer, not a failure
	// (some git versions exit 128 on it, so the message is the signal).
	if strings.Contains(stderr, "not a symbolic ref") {
		return "", true, nil
	}

	return "", false, err
}

func probeHash(ctx context.Context, dir string) (string, bool, error) {
	stdout, _, err := runGitProbe(ctx, dir, "rev-parse", "--short=12", "HEAD")
	if err == nil {
		h := strings.TrimSpace(stdout)
		if !validHash(h) {
			return "", false, errors.New("malformed HEAD hash")
		}

		return h, false, nil
	}

	// The failure message is version-dependent, so classify unborn HEAD by
	// protocol instead: --verify --quiet exits 1 exactly when HEAD (still)
	// points at no commit.
	if _, _, verifyErr := runGitProbe(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); isExitCode(verifyErr, 1) {
		return "", true, nil
	}

	return "", false, err
}

func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError

	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

func validHash(s string) bool {
	if len(s) < minHashLen || len(s) > maxHashLen {
		return false
	}

	for _, r := range s {
		_, err := strconv.ParseUint(string(r), 16, 8)
		if err != nil {
			return false
		}
	}

	return true
}

type statusCounts struct {
	staged     int
	unstaged   int
	untracked  int
	conflicted int
}

func probeStatus(ctx context.Context, dir string) (statusCounts, error) {
	// showUntrackedFiles=all pins the count against user config.
	stdout, _, err := runGitProbe(
		ctx,
		dir,
		"-c", "status.showUntrackedFiles=all",
		"status", "--porcelain=v1",
	)
	if err != nil {
		return statusCounts{}, err
	}

	return parsePorcelainStatus(stdout)
}

// parsePorcelainStatus counts entries by their XY code without retaining any
// path text. Untracked entries ("??") carry no staged/unstaged signal.
func parsePorcelainStatus(output string) (statusCounts, error) {
	var counts statusCounts

	for line := range strings.SplitSeq(output, "\n") {
		if line == "" {
			continue
		}

		if len(line) < 3 || line[2] != ' ' {
			return statusCounts{}, errors.New("malformed porcelain status line")
		}

		x, y := line[0], line[1]

		switch {
		case x == '?' && y == '?':
			counts.untracked++
		default:
			if x != ' ' && x != '?' {
				counts.staged++
			}

			if y != ' ' && y != '?' {
				counts.unstaged++
			}

			if x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D') {
				counts.conflicted++
			}
		}
	}

	return counts, nil
}

func (c statusCounts) applyTo(state *RepositoryState) {
	state.Staged = c.staged
	state.Unstaged = c.unstaged
	state.Untracked = c.untracked
	state.Conflicted = c.conflicted
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}

	return string([]rune(s)[:limit])
}
