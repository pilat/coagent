package bashsandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// probeFixture holds the directories one enforcement probe needs.
type probeFixture struct {
	base    string
	project string
	denied  string
	secret  string
}

// probeEnforcement checks that an ungranted directory is not writable while the
// granted project is.
func probeEnforcement(newRunner runnerFactory) error {
	fixture, err := newProbeFixture("coagent-sandbox-probe-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(fixture.base) }()

	policy, cleanup, err := compileProbePolicy(fixture.project, false)
	if err != nil {
		return err
	}
	defer cleanup()

	runner, err := newRunner(policy)
	if err != nil {
		return fmt.Errorf("create probe runner: %w", err)
	}

	if err := probeAllowedWrite(runner, fixture.project, fixture.project); err != nil {
		return err
	}

	return probeDeniedWrite(runner, fixture.project, fixture.denied)
}

func probeShieldEnforcement(newRunner runnerFactory) error {
	fixture, err := newProbeFixture("coagent-shields-probe-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(fixture.base) }()

	policy, cleanup, err := compileProbePolicy(fixture.project, true)
	if err != nil {
		return err
	}
	defer cleanup()

	runner, err := newRunner(policy)
	if err != nil {
		return fmt.Errorf("create shields probe runner: %w", err)
	}

	inside := filepath.Join(fixture.project, "inside")
	if output, err := runProbeIn(
		runner,
		"printf allowed >"+shellPath(inside)+" && cat "+shellPath(inside),
		fixture.project,
	); err != nil {
		return fmt.Errorf("shielded project access probe: %w: %s", err, output)
	}

	if output, err := runProbeIn(runner, "cat "+shellPath(fixture.secret), fixture.project); err == nil {
		return fmt.Errorf("shielded probe read denied path %q: %s", fixture.secret, output)
	}

	if output, err := runProbeIn(
		runner,
		"printf denied >"+shellPath(filepath.Join(fixture.denied, "write")),
		fixture.project,
	); err == nil {
		return fmt.Errorf("shielded probe wrote denied path %q: %s", fixture.denied, output)
	}

	return nil
}

// FixturePolicy compiles the policy a test or probe fixture executes under: the
// same construction a session uses, minus operator escalation, with the private
// temporary backing derived from the fixture's canonical root.
func FixturePolicy(project string, shields bool) (sandboxpolicy.Policy, error) {
	substrate, err := ExecutionSubstrate()
	if err != nil {
		return sandboxpolicy.Policy{}, err
	}

	catalog, err := sandboxpolicy.Load(nil)
	if err != nil {
		return sandboxpolicy.Policy{}, fmt.Errorf("load sandbox catalog: %w", err)
	}

	tempRoot, err := coagenthome.SandboxTempDir(coagenthome.SandboxPathIdentity(project))
	if err != nil {
		return sandboxpolicy.Policy{}, fmt.Errorf("resolve sandbox temp dir: %w", err)
	}

	compiled, err := sandboxpolicy.Compile(catalog, sandboxpolicy.Request{
		ProjectRoot: project, WorkDir: project, TempRoot: tempRoot, Shields: shields, Substrate: substrate,
	})
	if err != nil {
		return sandboxpolicy.Policy{}, fmt.Errorf("compile sandbox policy: %w", err)
	}

	return compiled, nil
}

// compileProbePolicy builds the policy one probe fixture executes under. The
// probe owns the temporary backing it creates, so it removes it afterwards.
func compileProbePolicy(project string, shields bool) (processPolicy, func(), error) {
	compiled, err := FixturePolicy(project, shields)
	if err != nil {
		return processPolicy{}, nil, err
	}

	policy, err := buildProcessPolicy(Config{
		Enabled: true, Shields: shields, Policy: compiled, WorkDir: project, SessionKey: "probe",
	})
	if err != nil {
		return processPolicy{}, nil, err
	}

	cleanup := func() {
		if compiled.TempRoot != "" {
			_ = os.RemoveAll(filepath.Dir(compiled.TempRoot))
		}
	}

	return policy, cleanup, nil
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
	fixture.secret = filepath.Join(fixture.denied, "secret")

	if err := os.Mkdir(fixture.project, 0o700); err != nil {
		_ = os.RemoveAll(base)

		return probeFixture{}, fmt.Errorf("create probe project: %w", err)
	}

	if err := os.Mkdir(fixture.denied, 0o700); err != nil {
		_ = os.RemoveAll(base)

		return probeFixture{}, fmt.Errorf("create probe denied directory: %w", err)
	}

	if err := os.WriteFile(fixture.secret, []byte("secret"), 0o600); err != nil {
		_ = os.RemoveAll(base)

		return probeFixture{}, fmt.Errorf("write probe denied file: %w", err)
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
