package bashsandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// probeFixture holds the directories one enforcement probe needs.
type probeFixture struct {
	base    string
	project string
	denied  string
}

// probeEnforcement checks that an ungranted directory is not writable while the
// granted project is.
func probeEnforcement(newRunner runnerFactory) error {
	fixture, err := newProbeFixture("coagent-sandbox-probe-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(fixture.base) }()

	policy, err := compileProbePolicy(fixture.project)
	if err != nil {
		return err
	}

	runner, err := newRunner(policy)
	if err != nil {
		return fmt.Errorf("create probe runner: %w", err)
	}

	if err := probeAllowedWrite(runner, fixture.project, fixture.project); err != nil {
		return err
	}

	return probeDeniedWrite(runner, fixture.project, fixture.denied)
}

// FixturePolicy compiles the policy a test or probe fixture executes under:
// the project readable and writable, with no operator rules.
func FixturePolicy(project string) (sandboxpolicy.Policy, error) {
	compiled, err := sandboxpolicy.Compile(sandboxpolicy.Request{ProjectRoot: project, WorkDir: project})
	if err != nil {
		return sandboxpolicy.Policy{}, fmt.Errorf("compile sandbox policy: %w", err)
	}

	return compiled, nil
}

// compileProbePolicy builds the policy one probe fixture executes under.
func compileProbePolicy(project string) (processPolicy, error) {
	compiled, err := FixturePolicy(project)
	if err != nil {
		return processPolicy{}, err
	}

	return buildProcessPolicy(Config{Enabled: true, Policy: compiled, WorkDir: project, SessionKey: "probe"})
}

func newProbeFixture(prefix string) (probeFixture, error) {
	base, err := os.MkdirTemp("", prefix)
	if err != nil {
		return probeFixture{}, fmt.Errorf("create probe directory: %w", err)
	}

	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		_ = os.RemoveAll(base)

		return probeFixture{}, fmt.Errorf("resolve probe directory: %w", err)
	}

	fixture := probeFixture{
		base: base, project: filepath.Join(base, "project"), denied: filepath.Join(base, "denied"),
	}

	if err := os.Mkdir(fixture.project, 0o700); err != nil {
		_ = os.RemoveAll(base)

		return probeFixture{}, fmt.Errorf("create probe project: %w", err)
	}

	if err := os.Mkdir(fixture.denied, 0o700); err != nil {
		_ = os.RemoveAll(base)

		return probeFixture{}, fmt.Errorf("create probe denied directory: %w", err)
	}

	return fixture, nil
}

func probeAllowedWrite(runner Runner, workDir, dir string) error {
	path := filepath.Join(dir, "probe")

	output, err := runProbeIn(runner, "printf allowed >"+shellPath(path), workDir)
	if err != nil {
		return fmt.Errorf("write allowed probe: %w: %s", err, output)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("verify allowed probe: %w", err)
	}

	if strings.TrimSpace(string(content)) != "allowed" {
		return fmt.Errorf("verify allowed probe: unexpected content %q", content)
	}

	return nil
}

func probeDeniedWrite(runner Runner, workDir, dir string) error {
	path := filepath.Join(dir, "probe")

	output, err := runProbeIn(runner, "printf denied >"+shellPath(path), workDir)
	if err == nil {
		return fmt.Errorf("sandbox allowed write to denied probe path %q", path)
	}

	_, err = os.Stat(path)
	if err == nil {
		return fmt.Errorf("sandbox modified denied probe path %q: %s", path, output)
	}

	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect denied probe path %q: %w", path, err)
	}

	return nil
}

func runProbeIn(runner Runner, command, workDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	cmd, err := runner.BashCommand(ctx, command, workDir)
	if err != nil {
		return "", fmt.Errorf("construct probe command: %w", err)
	}

	var output limitedBuffer

	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = preflightTimeout

	err = cmd.Run()
	detail := strings.TrimSpace(output.String())

	if ctx.Err() != nil {
		return detail, ctx.Err()
	}

	if err != nil {
		return detail, fmt.Errorf("run probe command: %w", err)
	}

	return detail, nil
}
