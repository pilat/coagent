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
	executable string
	profile    string
	parameters []string
	roots      []string
	provider   shellenv.Provider
	policyKey  string
}

func newEnabledRunner(writableRoots []string, sessionKey ...string) (Runner, error) {
	info, err := os.Stat(seatbeltExecutable)
	if err != nil {
		return nil, fmt.Errorf("locate Seatbelt executable %q: %w", seatbeltExecutable, err)
	}

	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("seatbelt executable %q is not executable", seatbeltExecutable)
	}

	runner := &darwinRunner{
		executable: seatbeltExecutable,
		profile:    seatbeltProfile(len(writableRoots)),
		parameters: seatbeltParameters(writableRoots),
		roots:      writableRoots,
		policyKey:  policyKey(writableRoots, firstSessionKey(sessionKey)),
	}

	if err := preflight(runner); err != nil {
		return nil, fmt.Errorf("seatbelt backend unavailable: %w", err)
	}

	return runner, nil
}

func (r *darwinRunner) Command(
	ctx context.Context,
	request procexec.Request,
) (*exec.Cmd, error) {
	args := make([]string, 0, len(r.parameters)*2+5)
	for i, value := range r.parameters {
		args = append(args, "-D", seatbeltParamName(i)+"="+value)
	}

	args = append(args, "-p", r.profile, request.Path)
	args = append(args, request.Args...)

	cmd := exec.CommandContext(ctx, r.executable, args...)
	cmd.Dir = request.WorkDir
	cmd.Env = request.Env

	return cmd, nil
}

func (r *darwinRunner) BashCommand(ctx context.Context, command, workDir string, commandArgs ...string) (*exec.Cmd, error) {
	return r.Command(ctx, procexec.Request{
		Path:    "bash",
		Args:    append([]string{"-c", command}, commandArgs...),
		WorkDir: workDir,
	})
}

// ShellCommand runs a user command confined by Seatbelt, sourcing workDir's
// snapshot when one is available so the command sees the project's toolchain.
func (r *darwinRunner) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	shell, snap := snapshotFor(ctx, r.provider, workDir)

	prog, progArgs := "bash", []string{"-c", command}
	if snap != "" {
		prog = shell
		progArgs = []string{"-c", sourceLine(snap, command)}
	}

	return r.Command(ctx, procexec.Request{Path: prog, Args: progArgs, WorkDir: workDir})
}

func (r *darwinRunner) WritableRoots() []string {
	return append([]string(nil), r.roots...)
}

func (r *darwinRunner) PolicyKey() string { return r.policyKey }

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
