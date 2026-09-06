package bashsandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ReadScope uint8

const (
	HostReadable ReadScope = iota
	ProjectConfined
)

type policyMount struct {
	source    string
	target    string
	directory bool
}

type processPolicy struct {
	readScope     ReadScope
	workDir       string
	projectRoot   string
	writableRoots []string
	readMounts    []policyMount
	sessionKey    string
}

//nolint:wsl_v5 // Policy normalization stays adjacent to each fail-closed validation.
func buildProcessPolicy(cfg Config, writableRoots []string) (processPolicy, error) {
	workDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return processPolicy{}, fmt.Errorf("resolve project spelling: %w", err)
	}
	workDir = filepath.Clean(workDir)

	projectRoot := cfg.CanonicalWorkDir
	if projectRoot == "" {
		projectRoot, err = filepath.EvalSymlinks(workDir)
		if err != nil {
			return processPolicy{}, fmt.Errorf("resolve canonical project: %w", err)
		}
	} else {
		projectRoot = filepath.Clean(projectRoot)
		if !filepath.IsAbs(projectRoot) {
			return processPolicy{}, errors.New("canonical project path is not absolute")
		}
	}

	policy := processPolicy{
		readScope: cfg.ReadScope, workDir: workDir, projectRoot: projectRoot,
		writableRoots: append([]string(nil), writableRoots...), sessionKey: cfg.SessionKey,
	}
	if cfg.ReadScope == HostReadable {
		return policy, nil
	}

	policy.writableRoots = []string{projectRoot}
	policy.readMounts, err = executionSubstrate()
	if err != nil {
		return processPolicy{}, err
	}

	return policy, nil
}

//nolint:wsl_v5 // Discovery and deterministic ordering are one policy construction pass.
func executionSubstrate() ([]policyMount, error) {
	mounts := make(map[string]policyMount)
	for _, candidate := range runtimeDirectoryCandidates() {
		if err := addRuntimeMount(mounts, candidate, true); err != nil {
			return nil, err
		}
	}
	for _, candidate := range runtimeFileCandidates() {
		if err := addRuntimeMount(mounts, candidate, false); err != nil {
			return nil, err
		}
	}

	ordered := make([]policyMount, 0, len(mounts))
	for _, mount := range mounts {
		ordered = append(ordered, mount)
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, right := pathDepth(ordered[i].target), pathDepth(ordered[j].target)
		if left != right {
			return left < right
		}
		return ordered[i].target < ordered[j].target
	})

	return ordered, nil
}

//nolint:wsl_v5 // Each candidate is validated and canonicalized before insertion.
func addRuntimeMount(mounts map[string]policyMount, candidate string, directory bool) error {
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
	source = filepath.Clean(source)
	target := filepath.Clean(candidate)
	mounts[target] = policyMount{source: source, target: target, directory: directory}
	if source != target {
		mounts[source] = policyMount{source: source, target: source, directory: directory}
	}

	return nil
}

//nolint:wsl_v5 // Every normalized policy field feeds one deterministic identity.
func (p processPolicy) key() string {
	parts := make([]string, 0, 4+len(p.writableRoots)+len(p.readMounts))
	parts = append(parts,
		fmt.Sprintf("scope:%d", p.readScope), "session:"+p.sessionKey,
		"workdir:"+p.workDir, "project:"+p.projectRoot,
	)
	for _, root := range p.writableRoots {
		parts = append(parts, "write:"+root)
	}
	for _, mount := range p.readMounts {
		parts = append(parts, fmt.Sprintf("read:%t:%s:%s", mount.directory, mount.source, mount.target))
	}
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))

	return hex.EncodeToString(hash[:])
}

//nolint:wsl_v5 // Readability is the union of the project and explicit substrate mounts.
func (p processPolicy) containsReadable(path string) bool {
	if pathWithinRoot(path, p.projectRoot) {
		return true
	}
	for _, mount := range p.readMounts {
		if path == mount.source || mount.directory && pathWithinRoot(path, mount.source) {
			return true
		}
	}

	return false
}

//nolint:wsl_v5 // Executable resolution validates every representation before admission.
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
	if !p.containsReadable(path) {
		return "", fmt.Errorf("executable %q is outside the shielded project and runtime substrate", name)
	}

	return path, nil
}

//nolint:wsl_v5 // Both lexical and canonical paths must stay in the project.
func (p processPolicy) validateWorkDir(name string) error {
	if name == "" {
		return errorsNewOutsideWorkDir(name)
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
		return errorsNewOutsideWorkDir(name)
	}

	return nil
}

func errorsNewOutsideWorkDir(name string) error {
	return fmt.Errorf("process work directory %q is outside the shielded project", name)
}

func lookPath(name string) (string, error) {
	path, err := execLookPath(name)
	if err != nil {
		return "", fmt.Errorf("find executable %q: %w", name, err)
	}

	return path, nil
}
