package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/daemon"
	"github.com/pilat/coagent/internal/managers/cli"
)

func TestAppShutdown_StopsInReverseOrderAndContinuesAfterError(t *testing.T) {
	var got []string
	a := &app{}

	for _, component := range []string{"first", "second", "third"} {
		registerTestStop(a, &got, component)
	}

	a.shutdown(context.Background())

	assert.Equal(t, []string{"third", "second", "first"}, got)
}

func TestStartCore_RegistersDatabaseAndDaemonLifecycle(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	t.Cleanup(restore)

	a := &app{}
	core, err := startCore(
		context.Background(),
		a,
		lifecycleTestConfig(),
		nil,
		daemon.NewConfigApplier(nil, nil),
	)
	require.NoError(t, err)
	require.NotNil(t, core)
	require.NotNil(t, core.controller)
	require.NotNil(t, core.scheduleStore)
	require.NotNil(t, core.scheduleSender)
	require.NotNil(t, core.verdictSender)
	require.NotNil(t, core.secretResolver)

	assert.Equal(t, []string{"shellenv", "mcp.pool", "db", "daemon"}, stopNames(a))
	assert.FileExists(t, filepath.Join(home, coagenthome.DirName, coagenthome.DBFileName))

	projectDir := filepath.Join(home, coagenthome.DirName, coagenthome.ProjectsDirName, "lifecycle")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	controller := core.controller.ForManager(controllerapi.BuiltinCLIManagerID)
	sessionID, err := controller.CreateSession(context.Background(), controllerapi.SessionCreateData{
		WorkDir: projectDir,
	})
	require.NoError(t, err)
	_, err = core.verdictSender.GetSession(context.Background(), sessionID)
	require.NoError(t, err, "the open database must serve a known session")

	a.shutdown(context.Background())
	_, err = core.verdictSender.GetSession(context.Background(), sessionID)
	require.ErrorContains(t, err, "database is closed")
}

func TestStartCore_PartialStartLeavesOnlyCreatedComponentsForCleanup(t *testing.T) {
	restore := coagenthome.Override("")
	t.Cleanup(restore)

	a := &app{}
	core, err := startCore(
		context.Background(),
		a,
		&config.Config{},
		nil,
		daemon.NewConfigApplier(nil, nil),
	)

	require.Nil(t, core)
	require.Error(t, err)
	require.ErrorContains(t, err, "open database")
	assert.Equal(t, []string{"shellenv", "mcp.pool"}, stopNames(a))

	// The caller's deferred shutdown must be safe after any prefix of startCore.
	a.shutdown(context.Background())
}

// A restart reply must be observable only after the whole control plane is
// ready, and returning from runDaemon must have released its bound socket.
func TestRunDaemon_ReadinessPrecedesRestartAndDeferredShutdownClosesSocket(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "coa-life")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	restore := coagenthome.Override(home)
	t.Cleanup(restore)

	ops := configops.New(
		filepath.Join(home, coagenthome.DirName, coagenthome.ConfigFileName),
		filepath.Join(home, coagenthome.DirName, coagenthome.SecretsFileName),
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() {
		result <- runDaemon(ctx, startupState{cfg: &config.Config{}}, ops, nil)
	}()

	socket, err := ctl.SocketPath()
	require.NoError(t, err)
	client := waitForReadyChat(t, socket, result)
	t.Cleanup(func() { _ = client.Close() })

	var restart ctl.RestartResult
	require.NoError(t, client.Call(t.Context(), ctl.OpRestartDaemon, nil, &restart))
	assert.True(t, restart.Restarting)

	select {
	case err := <-result:
		require.ErrorIs(t, err, errRestartRequested)
	case <-time.After(10 * time.Second):
		t.Fatal("runDaemon did not drain after accepting the restart reply")
	}

	require.Eventually(t, func() bool {
		probe, err := ctl.Dial(context.Background(), socket)
		if err != nil {
			return true
		}

		_ = probe.Close()

		return false
	}, time.Second, 10*time.Millisecond, "deferred shutdown must close the control socket")
}

func waitForReadyChat(t *testing.T, socket string, result <-chan error) *ctl.Client {
	t.Helper()

	var (
		client   *ctl.Client
		returned bool
		runErr   error
	)
	require.Eventually(t, func() bool {
		select {
		case runErr = <-result:
			returned = true
			return true
		default:
		}

		candidate, err := ctl.Dial(context.Background(), socket)
		if err == nil {
			var opened cli.OpenResult
			err = candidate.Call(t.Context(), cli.OpChatOpen, struct{}{}, &opened)
			if err == nil {
				client = candidate
				return true
			}

			_ = candidate.Close()
		}

		return false
	}, 10*time.Second, 10*time.Millisecond, "local chat readiness")

	require.False(t, returned, "runDaemon stopped before the local chat became ready: %v", runErr)
	require.NotNil(t, client)

	return client
}

func registerTestStop(a *app, got *[]string, component string) {
	a.onStop(component, func(context.Context) error {
		*got = append(*got, component)
		if component == "second" {
			return errors.New("stop failed")
		}

		return nil
	})
}

func lifecycleTestConfig() *config.Config {
	return &config.Config{
		Model: "fake-model",
		UnifiedConfig: &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{
				"test": {Driver: "openai", BaseURL: "http://127.0.0.1:1"},
			},
			Models: []config.ModelEntry{{ID: "fake-model", Provider: "test"}},
		},
	}
}

func stopNames(a *app) []string {
	names := make([]string, 0, len(a.stops))
	for _, stop := range a.stops {
		names = append(names, stop.name)
	}

	return names
}
