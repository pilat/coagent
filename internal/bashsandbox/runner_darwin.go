//go:build darwin

package bashsandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/shellenv"
)

const (
	seatbeltExecutable  = "/usr/bin/sandbox-exec"
	seatbeltParamPrefix = "WRITABLE_"
)

var seatbeltWritableDevices = []string{
	"/dev/dtracehelper",
	"/dev/fd",
	"/dev/null",
	"/dev/random",
	"/dev/tty",
	"/dev/urandom",
	"/dev/zero",
}

var (
	_ Runner        = (*darwinRunner)(nil)
	_ providerAware = (*darwinRunner)(nil)
)

type darwinRunner struct {
	executable     string
	profile        string
	parameters     []string
	parameterNames []string
	roots          []string
	provider       shellenv.Provider
	policyKey      string
	policy         processPolicy
}

type seatbeltRule struct {
	name string
	path string
	dir  bool
}

func newEnabledRunner(policy processPolicy) (Runner, error) {
	info, err := os.Stat(seatbeltExecutable)
	if err != nil {
		return nil, fmt.Errorf("locate Seatbelt executable %q: %w", seatbeltExecutable, err)
	}

	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("seatbelt executable %q is not executable", seatbeltExecutable)
	}

	runner := &darwinRunner{
		executable: seatbeltExecutable,
		profile:    seatbeltProfile(len(policy.writableRoots)),
		parameters: seatbeltParameters(policy.writableRoots),
		roots:      policy.writableRoots,
		policyKey:  policy.key(),
		policy:     policy,
	}
	if policy.readScope == ProjectConfined {
		runner.profile, runner.parameterNames, runner.parameters = shieldedSeatbeltPolicy(policy)
	}

	if err := preflight(runner, policy.workDir); err != nil {
		return nil, fmt.Errorf("seatbelt backend unavailable: %w", err)
	}

	return runner, nil
}

func (r *darwinRunner) Command(
	ctx context.Context,
	request procexec.Request,
) (*exec.Cmd, error) {
	environment, err := innerEnvironment(request.Env, request.WorkDir)
	if err != nil {
		return nil, err
	}

	path := request.Path
	if r.policy.readScope == ProjectConfined {
		if err := r.policy.validateWorkDir(request.WorkDir); err != nil {
			return nil, err
		}

		var err error

		path, err = r.policy.resolveExecutable(path, request.WorkDir)
		if err != nil {
			return nil, err
		}
	}

	args := make([]string, 0, len(r.parameters)*2+5)
	for i, value := range r.parameters {
		name := seatbeltParamName(i)
		if len(r.parameterNames) == len(r.parameters) {
			name = r.parameterNames[i]
		}

		args = append(args, "-D", name+"="+value)
	}

	args = append(args, "-p", r.profile, "/usr/bin/env", "-i", "--")

	for _, entry := range environment {
		args = append(args, entry.raw)
	}

	if r.policy.readScope == ProjectConfined {
		args = append(args, "/bin/sh", "-c", `cd -- "$1" && shift && exec "$@"`,
			"coagent-shield", request.WorkDir, path)
	} else {
		args = append(args, path)
	}

	args = append(args, request.Args...)

	cmd := exec.CommandContext(ctx, r.executable, args...)
	if r.policy.readScope == ProjectConfined {
		cmd.Dir = r.policy.projectRoot
	} else {
		cmd.Dir = request.WorkDir
	}

	cmd.Env = sandboxLauncherEnvironment()

	return cmd, nil
}

func (r *darwinRunner) BashCommand(
	ctx context.Context,
	command, workDir string,
	commandArgs ...string,
) (*exec.Cmd, error) {
	return r.Command(ctx, procexec.Request{
		Path:    bashExecutable,
		Args:    append([]string{"-c", command}, commandArgs...),
		WorkDir: workDir,
	})
}

// ShellCommand runs a user command confined by Seatbelt, sourcing workDir's
// snapshot when one is available so the command sees the project's toolchain.
func (r *darwinRunner) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	shell, snap := snapshotFor(ctx, r.provider, workDir)

	prog, progArgs := bashExecutable, []string{"-c", command}
	if snap != "" {
		prog = shell
		progArgs = []string{"-c", sourceLine(snap, command)}
	}

	return r.Command(ctx, procexec.Request{Path: prog, Args: progArgs, WorkDir: workDir})
}

func (r *darwinRunner) WritableRoots() []string {
	return append([]string(nil), r.roots...)
}

func (r *darwinRunner) PolicyKey() string    { return r.policyKey }
func (r *darwinRunner) ReadScope() ReadScope { return r.policy.readScope }

func (r *darwinRunner) setProvider(p shellenv.Provider) { r.provider = p }

func seatbeltProfile(rootCount int) string {
	var profile strings.Builder
	profile.WriteString("(version 1)\n")
	profile.WriteString("(allow default)\n")
	profile.WriteString("(deny file-write*\n")
	profile.WriteString("  (require-not (require-any\n")

	for i := range rootCount {
		profile.WriteString("    (subpath (param \"")
		profile.WriteString(seatbeltParamName(i))
		profile.WriteString("\"))\n")
	}

	for _, path := range seatbeltWritableDevices {
		profile.WriteString("    (subpath \"")
		profile.WriteString(path)
		profile.WriteString("\")\n")
	}

	profile.WriteString("  )))\n")

	return profile.String()
}

func seatbeltParameters(writableRoots []string) []string {
	return append([]string(nil), writableRoots...)
}

func seatbeltParamName(index int) string {
	return seatbeltParamPrefix + strconv.Itoa(index)
}

func shieldedSeatbeltPolicy(policy processPolicy) (string, []string, []string) {
	readRules := shieldedSeatbeltRules(policy)
	var profile strings.Builder
	profile.WriteString("(version 1)\n(allow default)\n")
	profile.WriteString("(deny file-read* (require-not (require-any\n")
	writeSeatbeltRootRule(&profile)

	for _, rule := range readRules {
		writeSeatbeltPathRule(&profile, rule.name, rule.dir)
		writeSeatbeltPathAncestorsRule(&profile, rule.name)
	}

	writeSeatbeltDeviceRules(&profile)
	profile.WriteString("  )))\n")
	profile.WriteString("(deny file-read-data (require-not (require-any\n")
	writeSeatbeltRootRule(&profile)

	for _, rule := range readRules {
		writeSeatbeltPathRule(&profile, rule.name, rule.dir)
	}

	writeSeatbeltDeviceRules(&profile)
	profile.WriteString("  )))\n")
	profile.WriteString("(deny file-write* (require-not (require-any\n")
	writeSeatbeltPathRule(&profile, "PROJECT_ROOT", true)
	writeSeatbeltPathRule(&profile, "PROJECT_ALIAS", true)
	writeSeatbeltDeviceRules(&profile)
	profile.WriteString("  )))\n")

	names := make([]string, 0, len(readRules))
	values := make([]string, 0, len(readRules))

	for _, rule := range readRules {
		names = append(names, rule.name)
		values = append(values, rule.path)
	}

	return profile.String(), names, values
}

func shieldedSeatbeltRules(policy processPolicy) []seatbeltRule {
	readRules := []seatbeltRule{
		{name: "PROJECT_ROOT", path: policy.projectRoot, dir: true},
		{name: "PROJECT_ALIAS", path: policy.workDir, dir: true},
	}

	seen := map[string]bool{policy.projectRoot: true, policy.workDir: true}
	for index, mount := range policy.readMounts {
		for _, path := range []string{mount.source, mount.target} {
			if seen[path] {
				continue
			}

			seen[path] = true
			readRules = append(readRules, seatbeltRule{
				name: "READ_" + strconv.Itoa(index) + "_" + strconv.Itoa(len(readRules)),
				path: path,
				dir:  mount.directory,
			})
		}
	}

	return readRules
}

func writeSeatbeltDeviceRules(profile *strings.Builder) {
	for _, path := range seatbeltWritableDevices {
		profile.WriteString("    (subpath \"")
		profile.WriteString(path)
		profile.WriteString("\")\n")
	}
}

// Seatbelt reads the root directory while launching; denying this exact path aborts before exec.
func writeSeatbeltRootRule(profile *strings.Builder) {
	profile.WriteString("    (literal \"/\")\n")
}

func writeSeatbeltPathRule(profile *strings.Builder, name string, directory bool) {
	profile.WriteString("    (literal (param \"")
	profile.WriteString(name)
	profile.WriteString("\"))\n")

	if directory {
		profile.WriteString("    (subpath (param \"")
		profile.WriteString(name)
		profile.WriteString("\"))\n")
	}
}

func writeSeatbeltPathAncestorsRule(profile *strings.Builder, name string) {
	profile.WriteString("    (path-ancestors (param \"")
	profile.WriteString(name)
	profile.WriteString("\"))\n")
}
