package bashsandbox_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

const testcontainersIntegrationEnv = "COAGENT_TESTCONTAINERS_INTEGRATION"

type linuxTestBinary struct {
	packagePath string
	guestPath   string
}

func TestLinuxSandboxInTestcontainer(t *testing.T) {
	if os.Getenv(testcontainersIntegrationEnv) != "1" {
		t.Skip("set " + testcontainersIntegrationEnv + "=1 to run the Linux container integration")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}

	binaries := []linuxTestBinary{
		{packagePath: "./internal/bashsandbox", guestPath: "/tmp/coagent-bashsandbox.test"},
		{packagePath: "./internal/tool/builtin", guestPath: "/tmp/coagent-builtin.test"},
	}
	hostDir := t.TempDir()
	for i := range binaries {
		binaries[i].build(ctx, t, repoRoot, hostDir)
	}

	ctr, err := testcontainers.Run(ctx, "debian:12",
		testcontainers.WithCmd("sleep", "infinity"),
		testcontainers.WithHostConfigModifier(func(cfg *container.HostConfig) {
			cfg.Privileged = true
		}),
	)
	if err != nil {
		t.Fatalf("start privileged Debian test container: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)

	runContainer(ctx, t, ctr,
		"apt-get update && "+
			"DEBIAN_FRONTEND=noninteractive apt-get install -y bubblewrap passwd && "+
			"useradd -m -s /bin/bash tester",
	)

	for _, binary := range binaries {
		if err := ctr.CopyFileToContainer(
			ctx,
			binary.hostPath(hostDir),
			binary.guestPath,
			0o755,
		); err != nil {
			t.Fatalf("copy %s into Linux container: %v", binary.packagePath, err)
		}
	}

	runContainer(ctx, t, ctr,
		"su -s /bin/bash tester -c "+
			"'/tmp/coagent-bashsandbox.test -test.v -test.timeout 5m'",
	)
	runContainer(ctx, t, ctr,
		"su -s /bin/bash tester -c "+
			"\"/tmp/coagent-builtin.test -test.v -test.timeout 5m "+
			"-test.run '^Test(FilesystemTools_|SandboxFileMutator_|NewFileMutator_|DirectFileMutator)'\"",
	)
	runContainer(ctx, t, ctr,
		"COAGENT_BWRAP_MOUNT_INTEGRATION=1 "+
			"/tmp/coagent-bashsandbox.test -test.v -test.timeout 5m "+
			"-test.run '^TestBubblewrapIntegrationProtectsNestedMount$'",
	)
}

func (b linuxTestBinary) build(
	ctx context.Context,
	t *testing.T,
	repoRoot string,
	hostDir string,
) {
	t.Helper()

	cmd := exec.CommandContext(
		ctx,
		"go", "test", "-c",
		"-o", b.hostPath(hostDir),
		b.packagePath,
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+runtime.GOARCH,
		"CGO_ENABLED=0",
	)

	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build Linux test binary for %s: %v\n%s", b.packagePath, err, output)
	}
}

func (b linuxTestBinary) hostPath(hostDir string) string {
	return filepath.Join(hostDir, filepath.Base(b.guestPath))
}

func runContainer(
	ctx context.Context,
	t *testing.T,
	ctr testcontainers.Container,
	command string,
) {
	t.Helper()

	exitCode, outputReader, err := ctr.Exec(
		ctx,
		[]string{"bash", "-lc", command},
		tcexec.Multiplexed(),
	)
	if err != nil {
		t.Fatalf("execute Linux container command: %v\ncommand: %s", err, command)
	}

	output, err := io.ReadAll(outputReader)
	if err != nil {
		t.Fatalf("read Linux container command output: %v", err)
	}
	t.Logf("Linux container command output:\n%s", output)

	if exitCode != 0 {
		t.Fatalf("Linux container command exited with %d\ncommand: %s\n%s", exitCode, command, output)
	}
}
