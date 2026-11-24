package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/version"
)

func TestPrintStatus_MatchesTodaysOutput(t *testing.T) {
	tests := []struct {
		name       string
		cliVersion string
		status     *coagentv1.GetStatusResponse
		failure    error
		connected  bool
		wantExit   int
	}{
		{
			name:       "ready",
			cliVersion: "v1.2.0",
			status: &coagentv1.GetStatusResponse{
				Pid:           41337,
				UptimeSeconds: 8040,
				BinaryVersion: "v1.1.0",
				ConfigPath:    "/home/operator/.coagent/config.yaml",
				ConfigPresent: true,
				Providers: []*coagentv1.ProviderStatus{
					{Name: "fallback", Driver: "openrouter"},
					{Name: "work", Driver: "anthropic"},
				},
				ModelCount:   3,
				DefaultModel: "",
				StaleModels:  []string{"lost"},
				Managers: []*coagentv1.ManagerStatus{
					{Id: "main", Driver: "telegram", Enabled: true, Running: true},
					{Id: "retry", Driver: "telegram", Enabled: true, Error: "unauthorized"},
					{Id: "off", Driver: "telegram"},
				},
			},
			wantExit: 0,
		},
		{
			name:       "unconfigured",
			cliVersion: "v1.1.0",
			status: &coagentv1.GetStatusResponse{
				Pid:           41337,
				UptimeSeconds: 8040,
				BinaryVersion: "v1.1.0",
				ConfigPath:    "/home/operator/.coagent/config.yaml",
				ConfigPresent: false,
			},
			wantExit: 0,
		},
		{
			name:     "not_running",
			failure:  errors.New("dial refused"),
			wantExit: 2,
		},
		{
			name:      "starting",
			failure:   connect.NewError(connect.CodeUnavailable, "daemon is starting").WithRemote(),
			connected: true,
			wantExit:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalVersion := version.Version
			version.Version = tt.cliVersion
			t.Cleanup(func() { version.Version = originalVersion })

			var exitCode int
			got := captureStatusStdout(t, func() {
				if tt.failure != nil {
					exitCode = reportStatusFailure(tt.failure, tt.connected)

					return
				}

				printStatus(tt.status)
			})
			assert.Equal(t, tt.wantExit, exitCode)

			path := filepath.Join("testdata", "status_"+tt.name+".golden")
			want, err := os.ReadFile(path)
			require.NoError(t, err, "read golden %s; actual stdout:\n%s", path, got)
			assert.Equal(t, string(want), got)
		})
	}
}

func captureStatusStdout(t *testing.T, render func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	originalStdout := os.Stdout
	t.Cleanup(func() {
		os.Stdout = originalStdout
		_ = writer.Close()
		_ = reader.Close()
	})
	os.Stdout = writer
	render()
	require.NoError(t, writer.Close())
	os.Stdout = originalStdout

	output, err := io.ReadAll(reader)
	require.NoError(t, err)

	return string(output)
}
