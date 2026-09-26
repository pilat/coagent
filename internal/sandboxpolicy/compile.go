package sandboxpolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
)

// Request is the authority input for one project's policy generation.
type Request struct {
	ProjectRoot      string
	ProjectID        int64
	WorkDir          string
	TempRoot         string
	Shields          bool
	GlobalEscalated  []string
	ProjectEscalated []string
	LegacyWritable   []string
	Environment      map[string]string
	GitMetadataRoot  string
	Substrate        []SubstrateMount

	// ProcessOutputRoots are the owning session tree's recorded process-output
	// directories, admitted read-only so the tool-result contract keeps working
	// without exposing any sibling session's artifacts.
	ProcessOutputRoots []string
}

// SubstrateMount is one read-only base mount: a canonical host source and the
// sandbox path it must also appear at.
type SubstrateMount struct {
	Source string
	Target string
}

// Grant is one materializable filesystem grant. Presence is not authority: an
// absent declaration stays authorized and is materialized per spawn.
type Grant struct {
	Profile string
	Level   Level
	Source  string
	Target  string
	Mode    Mode
	Kind    ObjectKind
	Present bool
}

// SocketGrant is one exact pathname socket grant.
type SocketGrant struct {
	Profile string
	Level   Level
	Path    string
	Present bool
}

// Policy is the compiled authority for one project and shield state.
type Policy struct {
	ProjectRoot    string
	ProjectID      int64
	WorkDir        string
	TempRoot       string
	VarTempRoot    string
	Shields        bool
	CatalogVersion string
	Grants         []Grant
	Sockets        []SocketGrant
	Network        []Network
	Digest         string
}

// Compile turns a project's declared authority into a deterministic policy.
// Raised shields drop every profile entry — basic and escalated — while the
// base filesystem, linked-worktree Git metadata and private temporary storage
// remain.
func Compile(catalog Catalog, req Request) (Policy, error) {
	projectRoot, err := resolveExistingDir(req.ProjectRoot, "project root")
	if err != nil {
		return Policy{}, err
	}

	workDir, err := resolveExistingDir(req.WorkDir, "work directory")
	if err != nil {
		return Policy{}, err
	}

	tempRoot, err := validateTempRoot(req.TempRoot)
	if err != nil {
		return Policy{}, err
	}

	grants, err := baseGrants(req, projectRoot, workDir, tempRoot)
	if err != nil {
		return Policy{}, err
	}

	policy := Policy{
		ProjectRoot: projectRoot, ProjectID: req.ProjectID, WorkDir: workDir,
		TempRoot: tempRoot, VarTempRoot: varTempRoot(tempRoot), Shields: req.Shields,
		CatalogVersion: catalog.Version(),
	}

	if !req.Shields {
		profileEntries, err := compileProfiles(catalog, req)
		if err != nil {
			return Policy{}, err
		}

		legacy, err := compileLegacy(req.LegacyWritable, req.Environment)
		if err != nil {
			return Policy{}, err
		}

		grants = append(grants, profileEntries.mounts...)
		grants = append(grants, legacy...)
		policy.Sockets = profileEntries.sockets
		policy.Network = profileEntries.network
	}

	policy.Grants = mergeGrants(grants)
	if err := validatePrivateRuntimeTargets(policy.Grants, policy.Sockets); err != nil {
		return Policy{}, err
	}

	policy.Digest = policy.digest()

	return policy, nil
}

func validatePrivateRuntimeTargets(grants []Grant, sockets []SocketGrant) error {
	for _, grant := range grants {
		if pathOverlapsPrivateRuntime(grant.Target) {
			return fmt.Errorf("mount target %q overlaps the private sandbox runtime", grant.Target)
		}

		if pathOverlapsRuntimeOverlay(grant.Target) && !isFixedRuntimeFile(grant) {
			return fmt.Errorf("mount target %q overlaps sandbox network runtime files", grant.Target)
		}
	}

	for _, socket := range sockets {
		if pathOverlapsPrivateRuntime(socket.Path) {
			return fmt.Errorf("socket %q overlaps the private sandbox runtime", socket.Path)
		}

		if pathOverlapsRuntimeOverlay(socket.Path) {
			return fmt.Errorf("socket %q overlaps sandbox network runtime files", socket.Path)
		}
	}

	return nil
}

func isFixedRuntimeFile(grant Grant) bool {
	if grant.Profile != "" || grant.Mode != ModeReadOnly || grant.Kind != KindFile {
		return false
	}

	return slices.Contains(runtimeOverlayFiles, grant.Target)
}

func pathOverlapsPrivateRuntime(path string) bool {
	path = filepath.Clean(path)

	return path == PrivateRuntimeDir ||
		strings.HasPrefix(path, PrivateRuntimeDir+string(filepath.Separator)) ||
		strings.HasPrefix(PrivateRuntimeDir, path+string(filepath.Separator))
}

// baseGrants builds the fixed base filesystem: the canonical project RW, its
// private temporary storage and the read-only execution substrate.
func baseGrants(req Request, projectRoot, workDir, tempRoot string) ([]Grant, error) {
	grants := []Grant{
		{Source: projectRoot, Target: projectRoot, Mode: ModeReadWrite, Kind: KindDir, Present: true},
	}

	for _, backing := range []struct{ source, target string }{
		{tempRoot, TempPath},
		{varTempRoot(tempRoot), VarTempPath},
	} {
		kind, present, err := probeObject(backing.source)
		if err != nil {
			return nil, fmt.Errorf("private temporary storage: %w", err)
		}

		grants = append(grants, Grant{
			Source: backing.source, Target: backing.target, Mode: ModeReadWrite,
			Kind: kind.resolved(), Present: present,
		})
	}

	if workDir != projectRoot {
		grants = append(
			grants,
			Grant{Source: workDir, Target: workDir, Mode: ModeReadWrite, Kind: KindDir, Present: true},
		)
	}

	gitMetadata, ok, err := gitMetadataGrant(req.GitMetadataRoot)
	if err != nil {
		return nil, err
	}

	if ok {
		grants = append(grants, gitMetadata)
	}

	substrate, err := substrateGrants(req.Substrate)
	if err != nil {
		return nil, err
	}

	grants = append(grants, substrate...)

	outputs, err := processOutputGrants(req.ProcessOutputRoots)
	if err != nil {
		return nil, err
	}

	return append(grants, outputs...), nil
}

// processOutputGrants admits the owning session tree's recorded process output
// read-only. It is a base construction path: the coagent home is otherwise never
// a grant, and only the named session directories are exposed.
func processOutputGrants(roots []string) ([]Grant, error) {
	grants := make([]Grant, 0, len(roots))

	for i, root := range roots {
		if root == "" {
			continue
		}

		anchor, err := normalizeRootAnchor(root)
		if err != nil {
			return nil, fmt.Errorf("process output root %d: %w", i, err)
		}

		kind, present, err := probeObject(anchor)
		if err != nil {
			return nil, fmt.Errorf("process output root %d: %w", i, err)
		}

		grants = append(grants, Grant{
			Source: anchor, Target: anchor, Mode: ModeReadOnly, Kind: kind.resolved(), Present: present,
		})
	}

	return grants, nil
}

// gitMetadataGrant grants the verified main repository's complete Git
// metadata in both shield states: a linked worktree's ordinary operation
// requires the shared object and ref store.
func gitMetadataGrant(path string) (Grant, bool, error) {
	if path == "" {
		return Grant{}, false, nil
	}

	anchor, err := normalizeRootAnchor(path)
	if err != nil {
		return Grant{}, false, fmt.Errorf("linked worktree git metadata: %w", err)
	}

	kind, present, err := probeObject(anchor)
	if err != nil {
		return Grant{}, false, fmt.Errorf("linked worktree git metadata: %w", err)
	}

	if present && kind != KindDir {
		return Grant{}, false, fmt.Errorf("linked worktree git metadata %q is not a directory", anchor)
	}

	return Grant{
		Source: anchor, Target: anchor, Mode: ModeReadWrite, Kind: KindDir, Present: present,
	}, true, nil
}

func substrateGrants(substrate []SubstrateMount) ([]Grant, error) {
	grants := make([]Grant, 0, len(substrate))

	for i, mount := range substrate {
		source, err := normalizeAnchor(mount.Source)
		if err != nil {
			return nil, fmt.Errorf("substrate mount %d: %w", i, err)
		}

		target, err := normalizeAnchor(mount.Target)
		if err != nil {
			return nil, fmt.Errorf("substrate mount %d: %w", i, err)
		}

		kind, present, err := probeObject(source)
		if err != nil {
			return nil, fmt.Errorf("substrate mount %d: %w", i, err)
		}

		grants = append(grants, Grant{
			Source: canonicalAnchor(source), Target: target, Mode: ModeReadOnly,
			Kind: kind.resolved(), Present: present,
		})
	}

	return grants, nil
}

// profileEntries is the compiled profile authority: mounts, sockets and
// network grants that survive the escalation filter.
type profileEntries struct {
	mounts  []Grant
	sockets []SocketGrant
	network []Network
}

// compileProfiles applies every basic entry of every known profile plus the
// escalated entries of the effective escalation set.
func compileProfiles(catalog Catalog, req Request) (profileEntries, error) {
	escalated, err := escalationSet(catalog, req.GlobalEscalated, req.ProjectEscalated)
	if err != nil {
		return profileEntries{}, err
	}

	var entries profileEntries

	for _, name := range catalog.Names() {
		profile, ok := catalog.Profile(name)
		if !ok {
			return profileEntries{}, fmt.Errorf("catalog lost profile %q", name)
		}

		_, profileEscalated := escalated[name]

		mounts, err := profileMountGrants(name, profile.Mounts, profileEscalated, req.Environment)
		if err != nil {
			return profileEntries{}, err
		}

		sockets, err := profileSocketGrants(name, profile.Sockets, profileEscalated, req.Environment)
		if err != nil {
			return profileEntries{}, err
		}

		entries.mounts = append(entries.mounts, mounts...)
		entries.sockets = append(entries.sockets, sockets...)
		entries.network = append(entries.network, filterNetwork(profile.Network, profileEscalated)...)
	}

	entries.sockets = mergeSockets(entries.sockets)
	entries.network = mergeNetwork(entries.network)

	return entries, nil
}

// escalationSet is the union of the global and per-project grants. A project
// cannot subtract a global grant, and no repository input reaches this set.
func escalationSet(catalog Catalog, global, project []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(global)+len(project))

	for _, names := range [][]string{global, project} {
		for _, name := range names {
			if _, ok := catalog.Profile(name); !ok {
				return nil, fmt.Errorf("escalation references unknown profile %q", name)
			}

			set[name] = struct{}{}
		}
	}

	return set, nil
}

func profileMountGrants(name string, mounts []Mount, escalated bool, environment map[string]string) ([]Grant, error) {
	grants := make([]Grant, 0, len(mounts))

	for _, mount := range mounts {
		if mount.Type == LevelEscalated && !escalated {
			continue
		}

		anchor, keep, err := resolveDeclaredPath(mount.Path, environment)
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}

		if !keep {
			continue
		}

		if err := validateGrantPath(anchor, false); err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}

		kind, present, err := probeObject(anchor)
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}

		if present && mount.Kind != "" && kind != mount.Kind {
			return nil, fmt.Errorf(
				"profile %q grants %q as %q but it is a %q", name, mount.Path, mount.Kind, kind,
			)
		}

		if !present {
			kind = mount.Kind.resolved()
		}

		source := canonicalAnchor(anchor)
		if err := validateGrantPath(source, false); err != nil {
			return nil, fmt.Errorf("profile %q resolves outside allowed roots: %w", name, err)
		}

		grants = append(grants, Grant{
			Profile: name, Level: mount.Type, Source: source, Target: anchor,
			Mode: mount.Mode, Kind: kind, Present: present,
		})
	}

	return grants, nil
}

func profileSocketGrants(
	name string,
	sockets []Socket,
	escalated bool,
	environment map[string]string,
) ([]SocketGrant, error) {
	grants := make([]SocketGrant, 0, len(sockets))

	for _, socket := range sockets {
		if socket.Type == LevelEscalated && !escalated {
			continue
		}

		anchor, keep, err := resolveDeclaredPath(socket.Path, environment)
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}

		if !keep {
			continue
		}

		if err := validateGrantPath(anchor, true); err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}

		present, err := probeSocket(anchor)
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}

		source := canonicalAnchor(anchor)
		if err := validateGrantPath(source, true); err != nil {
			return nil, fmt.Errorf("profile %q socket resolves outside allowed roots: %w", name, err)
		}

		grants = append(grants, SocketGrant{
			Profile: name, Level: socket.Type, Path: source, Present: present,
		})
	}

	return grants, nil
}

func filterNetwork(entries []Network, escalated bool) []Network {
	kept := make([]Network, 0, len(entries))

	for _, entry := range entries {
		if entry.Type == LevelEscalated && !escalated {
			continue
		}

		kept = append(kept, Network{
			Address: entry.Address, Protocol: entry.Protocol, Ports: sortedPorts(entry.Ports), Type: entry.Type,
		})
	}

	return kept
}

// compileLegacy translates the deprecated writable_paths input into ordinary
// read-write grants. It is an operator grant, never a model-reachable one.
func compileLegacy(paths []string, environment map[string]string) ([]Grant, error) {
	grants := make([]Grant, 0, len(paths))

	for i, path := range paths {
		anchor, keep, err := resolveDeclaredPath(path, environment)
		if err != nil {
			return nil, fmt.Errorf("sandbox.writable_paths %d: %w", i, err)
		}

		if !keep {
			continue
		}

		if err := validateGrantPath(anchor, false); err != nil {
			return nil, fmt.Errorf("sandbox.writable_paths %d: %w", i, err)
		}

		kind, present, err := probeObject(anchor)
		if err != nil {
			return nil, fmt.Errorf("sandbox.writable_paths %d: %w", i, err)
		}

		source := canonicalAnchor(anchor)
		if err := validateGrantPath(source, false); err != nil {
			return nil, fmt.Errorf("sandbox.writable_paths %d resolves outside allowed roots: %w", i, err)
		}

		grants = append(grants, Grant{
			Profile: legacyProfile, Level: LevelBasic, Source: source, Target: anchor,
			Mode: ModeReadWrite, Kind: kind.resolved(), Present: present,
		})
	}

	return grants, nil
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

// validateTempRoot anchors the private temporary backing under the coagent
// home, so a project temp grant can never point at an operator's own data.
func validateTempRoot(path string) (string, error) {
	if path == "" {
		return "", errors.New("private temporary storage is required")
	}

	anchor, err := normalizeAnchor(path)
	if err != nil {
		return "", fmt.Errorf("private temporary storage: %w", err)
	}

	controlDir, err := coagenthome.Dir()
	if err != nil {
		return "", fmt.Errorf("resolve coagent home: %w", err)
	}

	if !strings.HasPrefix(anchor, controlDir+string(filepath.Separator)) {
		return "", fmt.Errorf("private temporary storage %q is outside the coagent home", anchor)
	}

	return anchor, nil
}

func varTempRoot(tempRoot string) string {
	return filepath.Join(tempRoot, "var-tmp")
}

// probeSocket reports whether an anchor currently holds a Unix socket. A
// non-socket object at a declared socket anchor is a configuration error, not
// an absence.
func probeSocket(anchor string) (bool, error) {
	info, err := os.Lstat(anchor)
	if os.IsNotExist(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("inspect socket %q: %w", anchor, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("socket path %q is a symlink", anchor)
	}

	if info.Mode()&os.ModeSocket == 0 {
		return false, fmt.Errorf("socket path %q exists but is not a Unix socket", anchor)
	}

	return true, nil
}

func sortedPorts(ports []int) []int {
	sorted := append([]int(nil), ports...)
	sort.Ints(sorted)

	return sorted
}
