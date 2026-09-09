//go:build linux

package backgroundprocess

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
)

func TestGuardian_DaemonDeathKillsGroupAndRestartInterruptsOnce(t *testing.T) {
	ctx := context.Background()
	ledger := newTestStore(t)
	dbPath := sqlitePath(t, ledger.(*store).db)
	outputDir := t.TempDir()

	helper := exec.Command( //nolint:gosec // The current test binary is the hermetic helper.
		os.Args[0], "-test.run=^TestGuardianDaemonHelper$", "--", dbPath, outputDir,
	)
	helper.Env = append(isolatedGuardianEnv(os.Environ(), t.TempDir()), "COAGENT_TEST_GUARDIAN_DAEMON=1")
	stdout, err := helper.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, helper.Start())
	helperDone := false
	t.Cleanup(func() {
		if helperDone {
			return
		}

		_ = helper.Process.Kill()
		_ = helper.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan(), "daemon helper did not advertise its process")
	processID := strings.TrimSpace(scanner.Text())
	require.NotEmpty(t, processID)

	record := waitState(t, ledger, processID, StateRunning, 5*time.Second)
	commandPID := waitForCapturedPID(t, record.OutputPath)

	require.NoError(t, helper.Process.Kill())
	_ = helper.Wait()
	helperDone = true

	restarted := NewService(ledger, Options{
		OutputDir: outputDir, GuardianCommand: testGuardianCommand,
	})
	count, err := restarted.InterruptNonterminal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assertProcessGone(t, commandPID)

	final, err := ledger.GetProcess(ctx, processID)
	require.NoError(t, err)
	assert.Equal(t, StateInterrupted, final.State)

	var inputs int
	require.NoError(t, ledger.(*store).db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE session_id = ? AND source = 'process'
			AND json_extract(attributes, '$.process_id') = ?`, record.SessionID, processID).Scan(&inputs))
	assert.Equal(t, 1, inputs)

	count, err = restarted.InterruptNonterminal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestGuardianDaemonHelper(t *testing.T) {
	if os.Getenv("COAGENT_TEST_GUARDIAN_DAEMON") != "1" {
		return
	}

	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(os.Args) != separator+3 {
		fmt.Fprintln(os.Stderr, "invalid daemon helper arguments")
		os.Exit(1)
	}

	db, err := migrate.OpenDB(context.Background(), os.Args[separator+1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()

	service := NewService(NewStore(db), Options{
		OutputDir: os.Args[separator+2], GuardianCommand: testGuardianCommand,
	})
	record, err := service.Start(context.Background(), testSpec(2), func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", "echo $$; exec sleep 30"), nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	_, _ = fmt.Fprintln(os.Stdout, record.ID)
	select {}
}

func sqlitePath(t *testing.T, db *sql.DB) string {
	t.Helper()

	var sequence int
	var name, path string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT seq, name, file FROM pragma_database_list
		WHERE name = 'main'`).Scan(&sequence, &name, &path))

	return path
}

func waitForCapturedPID(t *testing.T, outputPath string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(outputPath)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("command pid was not captured in %s", outputPath)

	return 0
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err != nil {
			return
		}
		stat, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if readErr != nil || strings.Contains(string(stat), ") Z ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d survived daemon death", pid)
}
