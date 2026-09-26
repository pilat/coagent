package bashsandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	// Shields mirrors the compiled policy's shield state. It never selects the
	// authority: the policy's grants do, so an ordinary and a shielded session
	// run the same builder.
	Shields bool

	Policy     sandboxpolicy.Policy
	WorkDir    string
	SessionKey string

	// Network is the tree's network generation, or nil when the sandbox keeps
	// the daemon's network namespace.
	Network *NetworkLink
}

// processPolicy is the runner's view of one compiled policy generation.
type processPolicy struct {
	readScope     ReadScope
	workDir       string
	projectRoot   string
	grants        []sandboxpolicy.Grant
	sockets       []sandboxpolicy.SocketGrant
	writableRoots []string
	digest        string
	sessionKey    string
}

// ExecutionSubstrate discovers the fixed read-only system execution substrate:
// the runtime directories and files a Linux toolchain needs to execute. It is
// reviewed data, not a grant for all of /etc or the user's home.
func ExecutionSubstrate() ([]sandboxpolicy.SubstrateMount, error) {
	mounts := make(map[string]sandboxpolicy.SubstrateMount)

	for _, candidate := range runtimeDirectoryCandidates() {
		if err := addSubstrateMount(mounts, candidate, true); err != nil {
			return nil, err
		}
	}

	for _, candidate := range runtimeFileCandidates() {
		if err := addSubstrateMount(mounts, candidate, false); err != nil {
			return nil, err
		}
	}

	ordered := make([]sandboxpolicy.SubstrateMount, 0, len(mounts))
	for _, mount := range mounts {
		ordered = append(ordered, mount)
	}

	sort.Slice(ordered, func(i, j int) bool {
		left, right := pathDepth(ordered[i].Target), pathDepth(ordered[j].Target)
		if left != right {
			return left < right
		}

		return ordered[i].Target < ordered[j].Target
	})

	return ordered, nil
}

func addSubstrateMount(mounts map[string]sandboxpolicy.SubstrateMount, candidate string, directory bool) error {
	info, err := os.Stat(candidate)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("stat runtime path %q: %w", candidate, err)
	}

	if directory != info.IsDir() {
		return nil
	}

	source, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fmt.Errorf("resolve runtime path %q: %w", candidate, err)
	}

	target := filepath.Clean(candidate)
	mounts[target] = sandboxpolicy.SubstrateMount{Source: filepath.Clean(source), Target: target}

	if source != target {
		mounts[source] = sandboxpolicy.SubstrateMount{Source: filepath.Clean(source), Target: filepath.Clean(source)}
	}

	return nil
}

// buildProcessPolicy derives the process policy a runner executes under.
func buildProcessPolicy(cfg Config) (processPolicy, error) {
	workDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return processPolicy{}, fmt.Errorf("resolve work directory: %w", err)
	}

	if cfg.Enabled && len(cfg.Policy.Grants) == 0 {
		return processPolicy{}, errors.New("sandbox is enabled without a compiled policy")
	}

	if cfg.Policy.ProjectRoot == "" && len(cfg.Policy.Grants) > 0 {
		return processPolicy{}, errors.New("compiled sandbox policy has no project root")
	}

	// Every sandbox-enabled session runs the allowlisted policy; the scope only
	// distinguishes that from the operator's explicit sandbox opt-out.
	scope := HostReadable
	if len(cfg.Policy.Grants) > 0 {
		scope = ProjectConfined
	}

	return processPolicy{
		readScope:     scope,
		workDir:       filepath.Clean(workDir),
		projectRoot:   cfg.Policy.ProjectRoot,
		grants:        cfg.Policy.Grants,
		sockets:       cfg.Policy.Sockets,
		writableRoots: writableRootsOf(cfg.Policy),
		digest:        cfg.Policy.Digest,
		sessionKey:    cfg.SessionKey,
	}, nil
}

// writableRootsOf lists read-write grant targets with the project first:
// session tools treat the leading entry as the session project.
func writableRootsOf(policy sandboxpolicy.Policy) []string {
	roots := make([]string, 0, len(policy.Grants)+1)
	roots = append(roots, policy.ProjectRoot)

	for _, root := range policy.WritableRoots() {
		if root != policy.ProjectRoot {
			roots = append(roots, root)
		}
	}

	return roots
}

func (p processPolicy) key() string {
	return policyKey(p.digest, p.sessionKey)
}

// allowsRead reports whether the compiled policy grants read access to a path.
func (p processPolicy) allowsRead(path string) bool {
	for _, grant := range p.grants {
		if pathWithinRoot(path, grant.Target) {
			return true
		}
	}

	return false
}

// resolveExecutable validates an executable against the policy's read grants.
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
