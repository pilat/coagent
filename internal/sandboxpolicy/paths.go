package sandboxpolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
)

// protectedMountRoots are whole subtrees that never become a grant. A grant
// inside them (for example an explicit socket under /run) is still allowed;
// only the subtree itself is refused.
var (
	protectedMountRoots = []string{"/proc", "/sys", "/dev", "/run", "/tmp"}
	runtimeOverlayFiles = []string{"/etc/resolv.conf", "/etc/hosts"}
)

// homePlaceholder and envPlaceholder stand in for values that are only known
// when a policy is compiled. Validation works on the placeholder form so it
// never depends on the environment the daemon happens to run in.
const (
	homePlaceholder = "/__coagent_home__"
	envPlaceholder  = "/__coagent_env__"
	// PrivateRuntimeDir holds the pinned namespace entry inside each sandbox.
	PrivateRuntimeDir = "/run/coagent"
)

// ExpandPath resolves a leading ~/ against the user's home directory.
func ExpandPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path must not be empty")
	}

	if path != "~" && !strings.HasPrefix(path, "~/") {
		if strings.HasPrefix(path, "~") {
			return "", fmt.Errorf("path %q uses unsupported home expansion", path)
		}

		return path, nil
	}

	home, err := coagenthome.UserHome()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}

	if path == "~" {
		return home, nil
	}

	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

// CanonicalProjectKey resolves a configured project key to the canonical root
// it selects. Matching is exact; /gwt inheritance is selected by the caller.
func CanonicalProjectKey(key string) (string, error) {
	anchor, err := normalizeRootAnchor(key)
	if err != nil {
		return "", err
	}

	if err := validateBroadRoot(anchor); err != nil {
		return "", err
	}

	return canonicalAnchor(anchor), nil
}

// validateDeclaredPath checks a declared grant path without resolving the home
// directory or any environment name.
func validateDeclaredPath(path string, socket bool) error {
	placeholder, err := placeholderPath(path)
	if err != nil {
		return err
	}

	if !filepath.IsAbs(placeholder) {
		return fmt.Errorf("path %q must be absolute or start with ~/", path)
	}

	if err := rejectDotDot(path, placeholder); err != nil {
		return err
	}

	if err := validateBroadRoot(placeholder); err != nil {
		return err
	}

	if pathOverlapsRuntimeOverlay(placeholder) {
		return fmt.Errorf("path %q overlaps sandbox network runtime files", path)
	}

	if socket {
		return nil
	}

	for _, protected := range protectedMountRoots {
		if filepath.Clean(placeholder) == protected {
			return fmt.Errorf("path %q grants the whole protected root %q", path, protected)
		}
	}

	return nil
}

// placeholderPath substitutes every environment reference and a leading ~ with
// an inert absolute prefix, so structural checks can run anywhere.
func placeholderPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path must not be empty")
	}

	placeholder, err := placeholderEnv(path)
	if err != nil {
		return "", err
	}

	if placeholder == "~" {
		return homePlaceholder, nil
	}

	if rest, ok := strings.CutPrefix(placeholder, "~/"); ok {
		return filepath.Join(homePlaceholder, rest), nil
	}

	if strings.HasPrefix(placeholder, "~") {
		return "", fmt.Errorf("path %q uses unsupported home expansion", path)
	}

	return placeholder, nil
}

// placeholderEnv replaces ${NAME} references with an inert absolute path and
// rejects a name outside the reviewed allowlist. Each name keeps a distinct
// placeholder so two different variables never look like the same path.
func placeholderEnv(path string) (string, error) {
	if !strings.Contains(path, "${") {
		return path, nil
	}

	var built strings.Builder

	rest := path
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			built.WriteString(rest)

			break
		}

		end := strings.Index(rest[start:], "}")
		if end < 0 {
			return "", fmt.Errorf("path %q has an unterminated environment reference", path)
		}

		name := rest[start+2 : start+end]
		if _, ok := catalogEnvDefaults[name]; !ok {
			return "", fmt.Errorf("path %q references unknown environment name %q", path, name)
		}

		built.WriteString(rest[:start])
		built.WriteString(envPlaceholder)
		built.WriteString(strings.ReplaceAll(name, "/", "_"))

		rest = rest[start+end+1:]
	}

	return built.String(), nil
}

// rejectDotDot refuses traversal components: a declared anchor must name the
// object it appears to name.
func rejectDotDot(path, _ string) error {
	if slices.Contains(strings.Split(path, string(filepath.Separator)), "..") {
		return fmt.Errorf("path %q contains a traversal component", path)
	}

	return nil
}

// resolveDeclaredPath expands environment references and the home directory for
// a declared path. An unset name with no default drops the entry.
func resolveDeclaredPath(path string, environment map[string]string) (string, bool, error) {
	expanded, keep, err := expandDeclaredEnv(path, environment)
	if err != nil || !keep {
		return "", keep, err
	}

	anchor, err := normalizeAnchor(expanded)
	if err != nil {
		return "", false, err
	}

	return anchor, true, nil
}

// expandDeclaredEnv substitutes ${NAME} references from the reviewed allowlist
// using the daemon's start environment.
func expandDeclaredEnv(path string, environment map[string]string) (string, bool, error) {
	if !strings.Contains(path, "${") {
		return path, true, nil
	}

	var built strings.Builder

	rest := path
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			built.WriteString(rest)

			break
		}

		end := strings.Index(rest[start:], "}")
		if end < 0 {
			return "", false, fmt.Errorf("path %q has an unterminated environment reference", path)
		}

		name := rest[start+2 : start+end]

		value, ok := catalogEnvValue(name, environment)
		if !ok {
			return "", false, fmt.Errorf("path %q references unknown environment name %q", path, name)
		}

		if value == "" {
			return "", false, nil
		}

		built.WriteString(rest[:start])
		built.WriteString(value)

		rest = rest[start+end+1:]
	}

	expanded := built.String()
	if !strings.HasPrefix(expanded, "/") && !strings.HasPrefix(expanded, "~") {
		return "", false, fmt.Errorf("expanded path %q from %q is not absolute", expanded, path)
	}

	return expanded, true, nil
}

// normalizeAnchor expands and cleans a declared path without following
// symlinks: an anchor must stay meaningful while the object is absent.
func normalizeAnchor(path string) (string, error) {
	expanded, err := ExpandPath(path)
	if err != nil {
		return "", err
	}

	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("path %q must be absolute or start with ~/", path)
	}

	return filepath.Clean(expanded), nil
}

// normalizeRootAnchor is normalizeAnchor for paths that must address a real
// directory root, such as a canonical project root.
func normalizeRootAnchor(path string) (string, error) {
	anchor, err := normalizeAnchor(path)
	if err != nil {
		return "", err
	}

	if anchor == string(filepath.Separator) {
		return "", fmt.Errorf("path %q resolves to the filesystem root", path)
	}

	return anchor, nil
}

// validateGrantPath rejects the broad roots no profile may grant.
func validateGrantPath(path string, socket bool) error {
	anchor, err := normalizeAnchor(path)
	if err != nil {
		return err
	}

	if err := validateBroadRoot(anchor); err != nil {
		return err
	}

	if pathOverlapsRuntimeOverlay(anchor) {
		return fmt.Errorf("path %q overlaps sandbox network runtime files", path)
	}

	if socket {
		return nil
	}

	for _, protected := range protectedMountRoots {
		if anchor == protected {
			return fmt.Errorf("path %q grants the whole protected root %q", path, protected)
		}
	}

	return nil
}

// validateBroadRoot rejects filesystem root, the home root and the coagent
// control/state directory.
func validateBroadRoot(anchor string) error {
	if anchor == PrivateRuntimeDir || strings.HasPrefix(anchor, PrivateRuntimeDir+string(filepath.Separator)) {
		return fmt.Errorf("path %q grants the private sandbox runtime", anchor)
	}

	if anchor == string(filepath.Separator) {
		return fmt.Errorf("path %q grants the filesystem root", anchor)
	}

	if anchor == filepath.Clean(homePlaceholder) {
		return fmt.Errorf("path %q grants the whole home directory", anchor)
	}

	if anchor == filepath.Clean(homePlaceholder+"/"+coagenthome.DirName) ||
		strings.HasPrefix(anchor, filepath.Clean(homePlaceholder+"/"+coagenthome.DirName)+string(filepath.Separator)) {
		return fmt.Errorf("path %q grants coagent control state", anchor)
	}

	// The placeholder covers a literal ~; an absolute spelling needs the
	// resolved home. An unresolvable home or control dir skips only this check.
	if home, err := coagenthome.UserHome(); err == nil && anchor == filepath.Clean(home) {
		return fmt.Errorf("path %q grants the whole home directory", anchor)
	}

	controlDir, err := coagenthome.Dir()
	if err == nil && (anchor == controlDir || strings.HasPrefix(anchor, controlDir+string(filepath.Separator))) {
		return fmt.Errorf("path %q grants coagent control state", anchor)
	}

	return nil
}

func pathOverlapsRuntimeOverlay(path string) bool {
	path = filepath.Clean(path)
	for _, runtimeFile := range runtimeOverlayFiles {
		if path == runtimeFile ||
			strings.HasPrefix(path, runtimeFile+string(filepath.Separator)) ||
			strings.HasPrefix(runtimeFile, path+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// canonicalAnchor resolves the longest existing prefix so an optional path
// cannot hide a protected parent behind a symlink.
func canonicalAnchor(anchor string) string {
	probe := anchor
	missing := make([]string, 0)

	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for _, part := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, part)
			}

			return filepath.Clean(resolved)
		}

		if !os.IsNotExist(err) || probe == "/" {
			return anchor
		}

		missing = append(missing, filepath.Base(probe))
		probe = filepath.Dir(probe)
	}
}

// probeObject reports the filesystem kind at an anchor. Absence is reported as
// (false, nil); a permission error is a real error, never a silent omission.
func probeObject(anchor string) (ObjectKind, bool, error) {
	info, err := os.Lstat(anchor)
	if os.IsNotExist(err) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("inspect %q: %w", anchor, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("path %q is a symlink and is not a mountable grant", anchor)
	}

	if info.IsDir() {
		return KindDir, true, nil
	}

	if info.Mode().IsRegular() {
		return KindFile, true, nil
	}

	return "", false, fmt.Errorf("path %q is neither a directory nor a regular file", anchor)
}
