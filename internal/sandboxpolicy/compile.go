package sandboxpolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Request is the input for one session's policy compilation.
type Request struct {
	ProjectRoot string
	ProjectID   int64
	WorkDir     string

	GlobalRules  []Rule
	ProjectRules []Rule

	// ProcessOutputRoot is daemon-owned and remains readable after operator rules.
	ProcessOutputRoot string

	// WorktreeGitDir is the main repository's .git directory when this session
	// runs in a worktree coagent created. Empty otherwise.
	WorktreeGitDir string
}

// Entry is one resolved, ordered authority statement.
type Entry struct {
	Path    string
	Action  Action
	Mode    Mode
	Kind    ObjectKind
	Present bool
}

// Policy is the compiled, ordered authority for one session.
type Policy struct {
	ProjectRoot string
	ProjectID   int64
	WorkDir     string
	Entries     []Entry
	Digest      string
}

// Refresh inspects the current mount object without changing the rule's authority.
func (e Entry) Refresh() (Entry, error) {
	kind, present, err := probeObject(e.Path)
	if err != nil {
		return Entry{}, err
	}

	e.Present = present
	if present {
		e.Kind = kind
	}

	return e, nil
}

// Compile orders global defaults and rules before project defaults and rules.
// Daemon-owned process output remains read-only after all operator rules.
func Compile(req Request) (Policy, error) {
	projectRoot, err := resolveExistingDir(req.ProjectRoot, "project root")
	if err != nil {
		return Policy{}, err
	}

	workDir, err := resolveExistingDir(req.WorkDir, "work directory")
	if err != nil {
		return Policy{}, err
	}

	globalEntries, err := resolveRuleGroup("sandbox.rules", req.GlobalRules)
	if err != nil {
		return Policy{}, err
	}

	projectDefaults, err := projectDefaultEntries(projectRoot, workDir, req.WorktreeGitDir)
	if err != nil {
		return Policy{}, err
	}

	projectEntries, err := resolveRuleGroup("sandbox.projects rules", req.ProjectRules)
	if err != nil {
		return Policy{}, err
	}

	entries := make([]Entry, 0, 1+len(globalEntries)+len(projectDefaults)+len(projectEntries))
	entries = append(
		entries,
		Entry{Path: string(filepath.Separator), Action: ActionAllow, Mode: ModeReadOnly, Kind: KindDir, Present: true},
	)
	entries = append(entries, globalEntries...)
	entries = append(entries, projectDefaults...)
	projectRulesStart := len(entries)
	entries = append(entries, projectEntries...)

	if err := requireProjectWritable(entries, projectRulesStart, projectRoot); err != nil {
		return Policy{}, err
	}

	if req.ProcessOutputRoot != "" {
		output, err := processOutputEntry(req.ProcessOutputRoot)
		if err != nil {
			return Policy{}, err
		}

		entries = append(entries, output)
	}

	policy := Policy{
		ProjectRoot: projectRoot,
		ProjectID:   req.ProjectID,
		WorkDir:     workDir,
		Entries:     entries,
	}

	policy.Digest = policy.digest()
	if err := policy.ValidateMounts(); err != nil {
		return Policy{}, err
	}

	return policy, nil
}

// ValidateMounts refuses absent objects whose omission would weaken authority.
func (p Policy) ValidateMounts() error {
	entries := p.EffectiveEntries()
	for i, entry := range entries {
		if entry.Present {
			continue
		}

		if entry.Action == ActionDeny {
			return fmt.Errorf("deny path %q does not exist", entry.Path)
		}

		action, mode, _, _ := decide(entries[:i], entry.Path)
		if entry.Mode == ModeReadOnly && action == ActionAllow && mode == ModeReadWrite {
			return fmt.Errorf("read-only path %q does not exist beneath a writable grant", entry.Path)
		}
	}

	return nil
}

func processOutputEntry(path string) (Entry, error) {
	anchor, err := normalizeRootAnchor(path)
	if err != nil {
		return Entry{}, fmt.Errorf("process output root: %w", err)
	}

	entry, err := resolveRule(Rule{Allow: anchor, Mode: ModeReadOnly})
	if err != nil {
		return Entry{}, fmt.Errorf("process output root: %w", err)
	}

	if !entry.Present || entry.Kind != KindDir {
		return Entry{}, fmt.Errorf("process output root %q must be an existing directory", anchor)
	}

	return entry, nil
}

// Work directories and linked-worktree metadata inherit the project's writable default.
func projectDefaultEntries(projectRoot, workDir, worktreeGitDir string) ([]Entry, error) {
	entries := []Entry{
		{Path: projectRoot, Action: ActionAllow, Mode: ModeReadWrite, Kind: KindDir, Present: true},
	}

	if workDir != projectRoot {
		entries = append(
			entries,
			Entry{Path: workDir, Action: ActionAllow, Mode: ModeReadWrite, Kind: KindDir, Present: true},
		)
	}

	if worktreeGitDir == "" {
		return entries, nil
	}

	gitEntry, err := resolveWorktreeGitDir(worktreeGitDir)
	if err != nil {
		return nil, err
	}

	return append(entries, gitEntry), nil
}

// A linked worktree needs write access to the main repository's shared Git metadata.
func resolveWorktreeGitDir(path string) (Entry, error) {
	anchor, err := normalizeRootAnchor(path)
	if err != nil {
		return Entry{}, fmt.Errorf("linked worktree git dir: %w", err)
	}

	kind, present, err := probeObject(anchor)
	if err != nil {
		return Entry{}, fmt.Errorf("linked worktree git dir: %w", err)
	}

	if present && kind != KindDir {
		return Entry{}, fmt.Errorf("linked worktree git dir %q is not a directory", anchor)
	}

	return Entry{Path: anchor, Action: ActionAllow, Mode: ModeReadWrite, Kind: KindDir, Present: present}, nil
}

// Broader project rules must preserve root writes; an exact-root rule may revoke them.
func requireProjectWritable(entries []Entry, projectRulesStart int, projectRoot string) error {
	action, mode, winner, ok := decide(entries, projectRoot)
	if ok && action == ActionAllow && mode == ModeReadWrite {
		return nil
	}

	if entries[winner].Path == projectRoot {
		return nil
	}

	ruleIndex := winner - projectRulesStart

	return fmt.Errorf(
		"sandbox rules leave the project root %q unwritable: rule %d (%s) applies after it",
		projectRoot, ruleIndex, describeEntry(entries[winner]),
	)
}

func describeEntry(entry Entry) string {
	if entry.Action == ActionDeny {
		return fmt.Sprintf("deny %q", entry.Path)
	}

	return fmt.Sprintf("allow %q mode %s", entry.Path, entry.Mode)
}

// resolveRuleGroup resolves one ordered rule list, wrapping errors so an
// operator can tell which list and index failed.
func resolveRuleGroup(label string, rules []Rule) ([]Entry, error) {
	entries := make([]Entry, 0, len(rules))

	for i, rule := range rules {
		entry, err := resolveRule(rule)
		if err != nil {
			return nil, fmt.Errorf("%s %d: %w", label, i, err)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// resolveRule expands and probes one declared rule. An absent object keeps its
// rule: presence is not authority, and the object may appear later.
func resolveRule(rule Rule) (Entry, error) {
	if err := validateRule(rule); err != nil {
		return Entry{}, err
	}

	action, path, err := ruleActionPath(rule)
	if err != nil {
		return Entry{}, err
	}

	anchor, err := normalizeAnchor(path)
	if err != nil {
		return Entry{}, err
	}

	kind, present, err := probeObject(anchor)
	if err != nil {
		return Entry{}, err
	}

	mode := ModeReadOnly
	if action == ActionAllow && rule.Mode == ModeReadWrite {
		mode = ModeReadWrite
	}

	return Entry{Path: anchor, Action: action, Mode: mode, Kind: kind.resolved(), Present: present}, nil
}

func ruleActionPath(rule Rule) (Action, string, error) {
	if rule.Allow != "" && rule.Deny != "" {
		return "", "", errors.New("rule sets both allow and deny")
	}

	if rule.Allow != "" {
		return ActionAllow, rule.Allow, nil
	}

	if rule.Deny != "" {
		return ActionDeny, rule.Deny, nil
	}

	return "", "", errors.New("rule sets neither allow nor deny")
}

// resolveExistingDir canonicalizes a path that must address a real directory.
func resolveExistingDir(path, what string) (string, error) {
	anchor, err := normalizeRootAnchor(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}

	resolved, err := filepath.EvalSymlinks(anchor)
	if err != nil {
		return "", fmt.Errorf("resolve %s %q: %w", what, path, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect %s %q: %w", what, path, err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("%s %q is not a directory", what, path)
	}

	return filepath.Clean(resolved), nil
}
