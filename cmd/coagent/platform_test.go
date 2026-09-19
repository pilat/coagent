package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureLinuxPlatform(t *testing.T) {
	require.NoError(t, ensureLinuxPlatform("linux"))

	for _, goos := range []string{"darwin", "windows"} {
		err := ensureLinuxPlatform(goos)
		require.Error(t, err, goos)
		require.ErrorIs(t, err, errUnsupportedPlatform)
		assert.Contains(t, err.Error(), goos)
	}
}

// testRunRecorder is the guardian/dispatch seam the guard tests replace: it
// records whether it was reached, which is the ordering the guard exists for.
type testRunRecorder struct {
	guardianCalled bool
	dispatchCalled bool
}

func TestRunWith_PlatformGuardRefusesBeforeGuardianAndDispatch(t *testing.T) {
	// The guard takes no configuration: rejection must hold with a sandbox
	// config present and with sandbox.enabled disabled, so the decision cannot
	// depend on sandbox.enabled.
	for _, goos := range []string{"darwin", "windows", "freebsd"} {
		t.Run(goos, func(t *testing.T) {
			rec := &testRunRecorder{}

			code := runWith(
				goos,
				[]string{"daemon"},
				func([]string) (bool, error) {
					rec.guardianCalled = true

					return true, nil
				},
				func(context.Context, []string) int {
					rec.dispatchCalled = true

					return exitOK
				},
			)

			assert.Equal(t, exitError, code)
			assert.False(t, rec.guardianCalled, "guardian must not run before the platform refusal")
			assert.False(t, rec.dispatchCalled, "dispatch must not run before the platform refusal")
		})
	}
}

// TestRunWith_GuardIgnoresSandboxConfig pins that the platform decision does
// not inspect configuration: even a config file disabling the sandbox cannot
// change the refusal, because runWith never loads it.
func TestRunWith_GuardIgnoresSandboxConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".coagent"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, ".coagent", "config.yaml"),
		[]byte("sandbox:\n  enabled: false\n"),
		0o600,
	))

	rec := &testRunRecorder{}

	code := runWith(
		"darwin",
		[]string{"daemon"},
		func([]string) (bool, error) {
			rec.guardianCalled = true

			return true, nil
		},
		func(context.Context, []string) int {
			rec.dispatchCalled = true

			return exitOK
		},
	)

	assert.Equal(t, exitError, code)
	assert.False(t, rec.guardianCalled)
	assert.False(t, rec.dispatchCalled)
}

func TestRunWith_LinuxReachesGuardian(t *testing.T) {
	rec := &testRunRecorder{}

	code := runWith(
		"linux",
		[]string{"daemon"},
		func([]string) (bool, error) {
			rec.guardianCalled = true

			return true, nil
		},
		func(context.Context, []string) int {
			rec.dispatchCalled = true

			return exitOK
		},
	)

	assert.Equal(t, exitOK, code)
	assert.True(t, rec.guardianCalled)
	assert.False(t, rec.dispatchCalled)
}

func TestRunWith_LinuxUnhandledGuardianFallsThroughToDispatch(t *testing.T) {
	rec := &testRunRecorder{}

	code := runWith(
		"linux",
		[]string{"version"},
		func([]string) (bool, error) {
			rec.guardianCalled = true

			return false, nil
		},
		func(_ context.Context, _ []string) int {
			rec.dispatchCalled = true

			return exitOK
		},
	)

	assert.Equal(t, exitOK, code)
	assert.True(t, rec.guardianCalled)
	assert.True(t, rec.dispatchCalled)
}

func TestRunWith_GuardianErrorPropagatesExitCode(t *testing.T) {
	rec := &testRunRecorder{}

	code := runWith(
		"linux",
		[]string{"daemon"},
		func([]string) (bool, error) {
			rec.guardianCalled = true

			return true, errors.New("guardian failed")
		},
		func(context.Context, []string) int {
			rec.dispatchCalled = true

			return exitOK
		},
	)

	assert.Equal(t, exitError, code)
	assert.False(t, rec.dispatchCalled)
}
