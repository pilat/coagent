package bashsandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// ReadScope is the filesystem authority class a session's tools run under.
type ReadScope uint8

const (
	HostReadable ReadScope = iota
	ProjectConfined
)

// Config configures session process confinement from a compiled policy.
type Config struct {
	Enabled bool

	Policy     sandboxpolicy.Policy
	WorkDir    string
	SessionKey string
}

// processPolicy is the runner's view of one compiled policy generation.
type processPolicy struct {
	policy        sandboxpolicy.Policy
	workDir       string
	projectRoot   string
	writableRoots []string
	sessionKey    string
}

// buildProcessPolicy derives the process policy a runner executes under.
func buildProcessPolicy(cfg Config) (processPolicy, error) {
	workDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return processPolicy{}, fmt.Errorf("resolve work directory: %w", err)
	}

	if cfg.Enabled && len(cfg.Policy.Entries) == 0 {
		return processPolicy{}, errors.New("sandbox is enabled without a compiled policy")
	}

	if cfg.Policy.ProjectRoot == "" && len(cfg.Policy.Entries) > 0 {
		return processPolicy{}, errors.New("compiled sandbox policy has no project root")
	}

	return processPolicy{
		policy:        cfg.Policy,
		workDir:       filepath.Clean(workDir),
		projectRoot:   cfg.Policy.ProjectRoot,
		writableRoots: writableRootsOf(cfg.Policy),
		sessionKey:    cfg.SessionKey,
	}, nil
}

// writableRootsOf puts the project first only while the effective policy allows writes.
func writableRootsOf(policy sandboxpolicy.Policy) []string {
	roots := make([]string, 0, len(policy.Entries)+1)
	if policy.ProjectRoot != "" && policy.AllowsWrite(policy.ProjectRoot) {
		roots = append(roots, policy.ProjectRoot)
	}

	for _, root := range policy.WritableRoots() {
		if root != policy.ProjectRoot {
			roots = append(roots, root)
		}
	}

	return roots
}

// writableLauncherPaths includes parents because directory writes permit replacement.
func writableLauncherPaths(policy sandboxpolicy.Policy, executable string) []string {
	var paths []string

	for path := executable; ; path = filepath.Dir(path) {
		if policy.AllowsWrite(path) {
			paths = append(paths, path)
		}

		if path == "/" {
			return paths
		}
	}
}

func (p processPolicy) key() string {
	return policyKey(p.policy.Digest, p.sessionKey)
}

// readScope reports the authority class this policy enforces: a disabled
// sandbox (no entries) reads the host, an enabled one is project-confined.
func (p processPolicy) readScope() ReadScope {
	if len(p.policy.Entries) == 0 {
		return HostReadable
	}

	return ProjectConfined
}

// allowsRead reports whether the compiled policy admits reading a path.
func (p processPolicy) allowsRead(path string) bool {
	if len(p.policy.Entries) == 0 {
		return true
	}

	return p.policy.AllowsRead(path)
}

// resolveExecutable validates an executable against the policy's read entries.
func (p processPolicy) resolveExecutable(name, workDir string) (string, error) {
	path := name
	var err error

	if !filepath.IsAbs(path) {
		if strings.ContainsRune(path, filepath.Separator) {
			path = filepath.Join(workDir, path)
		} else {
			path, err = lookPath(name)
			if err != nil {
				return "", err
			}
		}
	}

	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable %q: %w", name, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat executable %q: %w", name, err)
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("executable %q is not an executable regular file", name)
	}

	if !p.allowsRead(path) {
		return "", fmt.Errorf("executable %q is outside the sandbox's granted paths", name)
	}

	return path, nil
}

// validateWorkDir keeps process working directories inside the session project.
func (p processPolicy) validateWorkDir(name string) error {
	if name == "" {
		return errOutsideWorkDir(name)
	}

	abs, err := filepath.Abs(name)
	if err != nil {
		return fmt.Errorf("resolve process work directory: %w", err)
	}

	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("resolve process work directory: %w", err)
	}

	if !pathWithinRoot(canonical, p.projectRoot) {
		return errOutsideWorkDir(name)
	}

	return nil
}

func errOutsideWorkDir(name string) error {
	return fmt.Errorf("process work directory %q is outside the sandboxed project", name)
}

func lookPath(name string) (string, error) {
	path, err := execLookPath(name)
	if err != nil {
		return "", fmt.Errorf("find executable %q: %w", name, err)
	}

	return path, nil
}
