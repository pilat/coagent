package shellenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type environmentValue struct {
	name  string
	value string
}

// WrapExec builds a command that sources workDir's snapshot then exec-replaces
// the shell with argv, so the server inherits the activated env/PATH but runs as
// the server process (not a shell wrapper). With no snapshot it returns a plain
// exec of argv. Explicit extraEnv is exported after the snapshot so installer
// isolation settings win while the remaining activated environment survives.
func (p *provider) WrapExec(
	ctx context.Context,
	workDir string,
	argv, extraEnv []string,
) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("shellenv: empty argv")
	}

	values, err := parseEnvironment(extraEnv)
	if err != nil {
		return nil, err
	}

	env := overriddenEnv(os.Environ(), values)

	snap := p.Snapshot(ctx, workDir)
	if snap == "" {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workDir
		cmd.Env = env

		return cmd, nil
	}

	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}

	line := "source " + shellQuote(snap) + "; " + shellExports(values) + "exec " + strings.Join(quoted, " ")

	cmd := exec.CommandContext(ctx, p.shell, "-c", line)
	cmd.Dir = workDir
	cmd.Env = env

	return cmd, nil
}

func (p *provider) LookPath(ctx context.Context, workDir string, names []string) (string, error) {
	if len(names) == 0 {
		return "", errors.New("shellenv: empty executable list")
	}

	for _, name := range names {
		if name == "" || strings.ContainsAny(name, "/\\ \t\n") {
			return "", fmt.Errorf("shellenv: unsafe executable name %q", name)
		}
	}

	snap := p.Snapshot(ctx, workDir)
	if snap == "" {
		return lookPathWithoutSnapshot(workDir, names)
	}

	return p.lookPathFromSnapshot(ctx, workDir, snap, names)
}

func lookPathWithoutSnapshot(workDir string, names []string) (string, error) {
	// nosemgrep: coagent-no-direct-environment-read -- PATH is the user-owned lookup contract.
	pathValue := os.Getenv("PATH")

	for _, name := range names {
		if found := executableFromPath(workDir, pathValue, name); found != "" {
			return found, nil
		}
	}

	return "", fmt.Errorf("executable not found: %s", strings.Join(names, ", "))
}

func (p *provider) lookPathFromSnapshot(ctx context.Context, workDir, snap string, names []string) (string, error) {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = "type -P -- " + shellQuote(name)
	}

	cmd := exec.CommandContext(ctx, p.shell, "-c", "source "+shellQuote(snap)+"; "+strings.Join(parts, " || "))
	cmd.Dir = workDir

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("activated executable lookup: %w", err)
	}

	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", fmt.Errorf("executable not found: %s", strings.Join(names, ", "))
	}

	return resolveExecutablePath(workDir, value)
}

//nolint:wsl_v5 // Each PATH element needs its own workdir-relative check.
func executableFromPath(workDir, pathValue, name string) string {
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = "."
		}
		candidate, err := resolveExecutablePath(workDir, filepath.Join(directory, name))
		if err != nil {
			continue
		}
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}

	return ""
}

//nolint:wsl_v5 // Relative and absolute executable paths have distinct bases.
func resolveExecutablePath(workDir, value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("shellenv: invalid executable path")
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	absoluteWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve executable workdir: %w", err)
	}
	path, err := filepath.Abs(filepath.Join(absoluteWorkDir, value))
	if err != nil {
		return "", fmt.Errorf("make executable path absolute: %w", err)
	}

	return path, nil
}

//nolint:wsl_v5 // Environment entries are parsed once before any process is built.
func parseEnvironment(extraEnv []string) ([]environmentValue, error) {
	values := make([]environmentValue, 0, len(extraEnv))
	for _, item := range extraEnv {
		name, value, ok := strings.Cut(item, "=")
		if !ok || !environmentName.MatchString(name) || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("shellenv: invalid environment variable %q", item)
		}
		values = append(values, environmentValue{name: name, value: value})
	}

	return values, nil
}

func shellExports(extraEnv []environmentValue) string {
	var builder strings.Builder

	for _, item := range extraEnv {
		builder.WriteString("export ")
		builder.WriteString(item.name)
		builder.WriteByte('=')
		builder.WriteString(shellQuote(item.value))
		builder.WriteString("; ")
	}

	return builder.String()
}

func overriddenEnv(base []string, extra []environmentValue) []string {
	replacements := make(map[string]struct{}, len(extra))
	for _, item := range extra {
		replacements[item.name] = struct{}{}
	}

	result := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		name, _, _ := strings.Cut(item, "=")
		if _, replaced := replacements[name]; !replaced {
			result = append(result, item)
		}
	}

	for _, item := range extra {
		result = append(result, item.name+"="+item.value)
	}

	return result
}

// shellQuote single-quotes s for safe folding into a `-c` string. Own copy:
// mirrors bashsandbox.shellPath — importing it would invert the dep direction.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
