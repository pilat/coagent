package mcp

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/shellenv"
)

var errRecorded = errors.New("recorded spawn")

// recordingProvider records WrapExec calls and returns an error so the spawn
// fails immediately — no real subprocess, no MCP handshake to hang on.
type recordingProvider struct {
	mu      sync.Mutex
	calls   int
	workDir string
	argv    []string
	env     []string
}

func (p *recordingProvider) Snapshot(context.Context, shellenv.ConfinedRunner, string) string {
	return ""
}
func (p *recordingProvider) Shell() string { return "" }
func (p *recordingProvider) Fingerprint(string) string {
	return ""
}
func (p *recordingProvider) Invalidate(string) {}
func (p *recordingProvider) Close() error      { return nil }
func (p *recordingProvider) LookPath(context.Context, shellenv.ConfinedRunner, string, []string) (string, error) {
	return "", errRecorded
}

func (p *recordingProvider) WrapExec(
	_ context.Context,
	_ shellenv.ConfinedRunner,
	workDir string,
	argv, extraEnv []string,
) (*exec.Cmd, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	p.workDir = workDir
	p.argv = argv
	p.env = extraEnv

	return nil, errRecorded
}

func (p *recordingProvider) snapshot() (int, string, []string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls, p.workDir, p.argv, p.env
}

func TestNewClient_RoutesSpawnThroughProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prov := &recordingProvider{}
	cfg := ServerConfig{
		Command: "my-mcp-server",
		Args:    []string{"--stdio"},
		Env:     map[string]string{"SERVER_SECRET": "x"},
		WorkDir: "/pinned/workdir",
	}

	_, err := NewClient(ctx, "srv", cfg, prov, nil)
	require.Error(t, err)

	calls, workDir, argv, env := prov.snapshot()
	assert.Equal(t, 1, calls)
	assert.Equal(t, "/pinned/workdir", workDir)
	assert.Equal(t, []string{"my-mcp-server", "--stdio"}, argv)
	assert.Contains(t, env, "SERVER_SECRET=x", "server env must pass through as extraEnv")
}

func TestDirectSpawn_RoutesThroughProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prov := &recordingProvider{}

	svc, err := AcquireForWorkDir(
		ctx,
		map[string]ServerConfig{"srv": {Command: "my-mcp-server"}},
		"/pinned/workdir",
		prov,
		nil,
	)
	require.NoError(t, err) // direct start logs failures, never returns them
	require.NotNil(t, svc)

	t.Cleanup(svc.Stop)

	calls, workDir, _, _ := prov.snapshot()
	assert.GreaterOrEqual(t, calls, 1, "direct path must spawn via the provider")
	assert.Equal(t, "/pinned/workdir", workDir)
}
