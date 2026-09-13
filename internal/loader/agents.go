package loader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/safefile"
)

// contextArtifact is one resolved instruction file: the search role that found
// it, the winning candidate path, and the trimmed body.
type contextArtifact struct {
	role string
	path string
	body string
}

// LoadAgentsMD resolves the global, project, and local context artifacts, each
// by a first-non-empty-wins search, and merges the winners into labeled
// sections; the error joins every failed candidate without blanking the merge.
// Project and local candidates read through a project-confined root; global
// candidates are trusted daemon inputs read from the host.
func (s *svc) LoadAgentsMD(workDir string) (string, error) {
	var artifacts []contextArtifact
	var errs []error

	global, err := s.findGlobalArtifact()
	if err != nil {
		errs = append(errs, err)
	}

	if global != nil {
		artifacts = append(artifacts, *global)
	}

	// Project-confined root even without session shields: an escaping symlink
	// in a project candidate must be denied, not followed.
	access, err := safefile.New(workDir, safefile.ProjectConfined)
	if err != nil {
		errs = append(errs, fmt.Errorf("contain project context candidates: %w", err))

		return mergeContextArtifacts(artifacts), errors.Join(errs...)
	}

	defer func() { _ = access.Close() }()

	project, err := s.findProjectArtifact(workDir, access)
	if err != nil {
		errs = append(errs, err)
	}

	if project != nil {
		artifacts = append(artifacts, *project)
	}

	local, err := s.findLocalArtifact(workDir, access)
	if err != nil {
		errs = append(errs, err)
	}

	if local != nil {
		artifacts = append(artifacts, *local)
	}

	return mergeContextArtifacts(artifacts), errors.Join(errs...)
}

func (s *svc) findGlobalArtifact() (*contextArtifact, error) {
	return s.searchArtifact("global", globalContextCandidates(), nil)
}

func (s *svc) findProjectArtifact(workDir string, access safefile.Access) (*contextArtifact, error) {
	return s.searchArtifact("project", projectContextCandidates(workDir), access)
}

func (s *svc) findLocalArtifact(workDir string, access safefile.Access) (*contextArtifact, error) {
	return s.searchArtifact("local", localContextCandidates(workDir), access)
}

// searchArtifact returns the first non-empty candidate; any other read error
// is recorded and the search continues, so one failure never blanks the merge.
func (s *svc) searchArtifact(role string, candidates []string, access safefile.Access) (*contextArtifact, error) {
	var errs []error

	for _, path := range candidates {
		content, err := readSourceFile(path, access)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			errs = append(errs, fmt.Errorf("read context artifact %s: %w", path, err))

			continue
		}

		body := strings.TrimSpace(string(content))
		if body == "" {
			continue
		}

		return &contextArtifact{role: role, path: path, body: body}, errors.Join(errs...)
	}

	return nil, errors.Join(errs...)
}

// mergeContextArtifacts renders found artifacts as "[role] <path>" sections in
// global, project, local order, separated by one blank line.
func mergeContextArtifacts(artifacts []contextArtifact) string {
	if len(artifacts) == 0 {
		return ""
	}

	var b strings.Builder

	for i, artifact := range artifacts {
		if i > 0 {
			b.WriteString("\n\n")
		}

		b.WriteString("[" + artifact.role + "] " + renderArtifactPath(artifact.path) + "\n\n" + artifact.body)
	}

	return b.String()
}

// renderArtifactPath abbreviates the home prefix to "~" so the rendered
// context never carries the username; an unresolvable home leaves it as-is.
func renderArtifactPath(path string) string {
	home, err := coagenthome.UserHome()
	if err != nil {
		return path
	}

	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}

	return "~" + string(filepath.Separator) + rel
}
