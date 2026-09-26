package sandboxpolicy

import (
	"fmt"
	"path/filepath"
	"sort"
)

// ValidateSection checks an operator-supplied sandbox section against the
// catalog. It runs before a candidate config may replace the live file, so an
// unknown profile, malformed address or broad root is refused while the
// running configuration stays untouched.
func ValidateSection(section Section) error {
	return ValidateSectionWithEnvironment(section, nil)
}

// ValidateSectionWithEnvironment validates paths against a captured daemon
// environment rather than reading mutable process state during compilation.
func ValidateSectionWithEnvironment(section Section, environment map[string]string) error {
	catalog, err := Load(section.Profiles)
	if err != nil {
		return err
	}

	if err := validateEscalation("sandbox.escalated", section.Escalated, catalog); err != nil {
		return err
	}

	if err := validateProjects(section.Projects, catalog); err != nil {
		return err
	}

	if err := validateLegacyWritablePaths(section.WritablePaths); err != nil {
		return err
	}

	return validateResolvedMountAnchors(catalog, section.WritablePaths, environment)
}

func validateResolvedMountAnchors(catalog Catalog, legacy []string, environment map[string]string) error {
	for _, name := range catalog.Names() {
		profile, _ := catalog.Profile(name)
		for _, mount := range profile.Mounts {
			anchor, keep, err := resolveDeclaredPath(mount.Path, environment)
			if err != nil {
				return fmt.Errorf("profile %q: %w", name, err)
			}

			if keep {
				if err := validateGrantPath(canonicalAnchor(anchor), false); err != nil {
					return fmt.Errorf("profile %q: %w", name, err)
				}
			}
		}

		for _, socket := range profile.Sockets {
			anchor, keep, err := resolveDeclaredPath(socket.Path, environment)
			if err != nil {
				return fmt.Errorf("profile %q: %w", name, err)
			}

			if keep {
				if err := validateGrantPath(canonicalAnchor(anchor), true); err != nil {
					return fmt.Errorf("profile %q: %w", name, err)
				}
			}
		}
	}

	for i, path := range legacy {
		anchor, keep, err := resolveDeclaredPath(path, environment)
		if err != nil {
			return fmt.Errorf("sandbox.writable_paths %d: %w", i, err)
		}

		if keep {
			if err := validateGrantPath(canonicalAnchor(anchor), false); err != nil {
				return fmt.Errorf("sandbox.writable_paths %d: %w", i, err)
			}
		}
	}

	return nil
}

// ValidateProfile checks one operator-defined profile definition.
func ValidateProfile(name string, profile Profile) error {
	return validateProfile(name, profile)
}

// validateProfile rejects malformed entries and contradictory duplicates.
func validateProfile(name string, profile Profile) error {
	if err := validateMounts(name, profile.Mounts); err != nil {
		return err
	}

	if err := validateSockets(name, profile.Sockets); err != nil {
		return err
	}

	return validateNetwork(name, profile.Network)
}

func validateMounts(name string, mounts []Mount) error {
	modes := make(map[string]map[Level]Mode, len(mounts))

	for i, mount := range mounts {
		if !mount.Type.valid() {
			return fmt.Errorf(
				"profile %q mount %d has invalid type %q, must be one of: basic, escalated",
				name,
				i,
				mount.Type,
			)
		}

		if !mount.Mode.valid() {
			return fmt.Errorf("profile %q mount %d has invalid mode %q, must be one of: ro, rw", name, i, mount.Mode)
		}

		if !mount.Kind.valid() {
			return fmt.Errorf("profile %q mount %d has invalid kind %q, must be one of: dir, file", name, i, mount.Kind)
		}

		if err := validateDeclaredPath(mount.Path, false); err != nil {
			return fmt.Errorf("profile %q mount %d: %w", name, i, err)
		}

		anchor, err := placeholderPath(mount.Path)
		if err != nil {
			return fmt.Errorf("profile %q mount %d: %w", name, i, err)
		}

		levels := modes[anchor]
		if levels == nil {
			levels = make(map[Level]Mode, 2)
			modes[anchor] = levels
		}

		if previous, ok := levels[mount.Type]; ok && previous != mount.Mode {
			return fmt.Errorf("profile %q grants %q in both %q and %q", name, mount.Path, previous, mount.Mode)
		}

		levels[mount.Type] = mount.Mode
		if basic, hasBasic := levels[LevelBasic]; hasBasic {
			if escalated, hasEscalated := levels[LevelEscalated]; hasEscalated &&
				basic != escalated && (basic != ModeReadOnly || escalated != ModeReadWrite) {
				return fmt.Errorf("profile %q grants %q in both %q and %q", name, mount.Path, basic, escalated)
			}
		}
	}

	return nil
}

func validateSockets(name string, sockets []Socket) error {
	levels := make(map[string]Level, len(sockets))

	for i, socket := range sockets {
		if !socket.Type.valid() {
			return fmt.Errorf(
				"profile %q socket %d has invalid type %q, must be one of: basic, escalated",
				name, i, socket.Type,
			)
		}

		if err := validateDeclaredPath(socket.Path, true); err != nil {
			return fmt.Errorf("profile %q socket %d: %w", name, i, err)
		}

		anchor, err := placeholderPath(socket.Path)
		if err != nil {
			return fmt.Errorf("profile %q socket %d: %w", name, i, err)
		}

		if previous, ok := levels[anchor]; ok {
			if previous != socket.Type {
				return fmt.Errorf(
					"profile %q grants socket %q as both %q and %q",
					name,
					socket.Path,
					previous,
					socket.Type,
				)
			}

			continue
		}

		levels[anchor] = socket.Type
	}

	return nil
}

func validateNetwork(name string, entries []Network) error {
	levels := make(map[string]Level, len(entries))

	for i, entry := range entries {
		if !entry.Type.valid() {
			return fmt.Errorf(
				"profile %q network %d has invalid type %q, must be one of: basic, escalated",
				name, i, entry.Type,
			)
		}

		if err := validateNetworkEntry(entry); err != nil {
			return fmt.Errorf("profile %q network %d: %w", name, i, err)
		}

		key := networkKey(entry)
		if previous, ok := levels[key]; ok {
			if previous != entry.Type {
				return fmt.Errorf("profile %q grants %s as both %q and %q", name, key, previous, entry.Type)
			}

			continue
		}

		levels[key] = entry.Type
	}

	return nil
}

func validateEscalation(field string, names []string, catalog Catalog) error {
	seen := make(map[string]struct{}, len(names))

	for _, name := range names {
		if name == "" {
			return fmt.Errorf("%s contains an empty profile name", field)
		}

		if _, ok := catalog.Profile(name); !ok {
			return fmt.Errorf("%s references unknown profile %q", field, name)
		}

		if _, ok := seen[name]; ok {
			return fmt.Errorf("%s lists profile %q twice", field, name)
		}

		seen[name] = struct{}{}
	}

	return nil
}

func validateProjects(projects map[string]ProjectOverride, catalog Catalog) error {
	canonical := make(map[string]string, len(projects))

	for key, override := range projects {
		anchor, err := placeholderPath(key)
		if err != nil {
			return fmt.Errorf("sandbox.projects key %q: %w", key, err)
		}

		if !filepath.IsAbs(anchor) {
			return fmt.Errorf("sandbox.projects key %q must be absolute or start with ~/", key)
		}

		if err := rejectDotDot(key, anchor); err != nil {
			return fmt.Errorf("sandbox.projects key %q: %w", key, err)
		}

		if err := validateBroadRoot(anchor); err != nil {
			return fmt.Errorf("sandbox.projects key %q: %w", key, err)
		}

		field := fmt.Sprintf("sandbox.projects[%q].escalated", key)
		if err := validateEscalation(field, override.Escalated, catalog); err != nil {
			return err
		}

		// Two spellings of one project must be refused: resolve aliases through
		// symlinks when the path exists, and compare the placeholder form
		// otherwise so validation never needs the environment.
		resolved, err := CanonicalProjectKey(key)
		if err != nil {
			return fmt.Errorf("sandbox.projects key %q: %w", key, err)
		}

		if previous, ok := canonical[resolved]; ok {
			return fmt.Errorf("sandbox.projects keys %q and %q select the same project", previous, key)
		}

		canonical[resolved] = key
	}

	return nil
}

// validateLegacyWritablePaths checks the deprecated writable_paths input: it
// survives as an operator grant, but not as a broad-root shortcut.
func validateLegacyWritablePaths(paths []string) error {
	for i, path := range paths {
		if err := validateDeclaredPath(path, false); err != nil {
			return fmt.Errorf("sandbox.writable_paths %d: %w; replace it with a sandbox profile mount entry", i, err)
		}
	}

	return nil
}

func networkKey(entry Network) string {
	ports := append([]int(nil), entry.Ports...)
	sort.Ints(ports)

	return fmt.Sprintf("%s/%s%v", entry.Address, entry.Protocol, ports)
}
