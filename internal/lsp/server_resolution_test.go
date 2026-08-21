package lsp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingProvider struct {
	path       string
	lookedUp   [][]string
	wrappedArg [][]string
}

func (p *recordingProvider) Snapshot(context.Context, string) string { return "" }

func (p *recordingProvider) Shell() string { return "" }

func (p *recordingProvider) Fingerprint(string) string { return "" }

func (p *recordingProvider) Invalidate(string) {}

func (p *recordingProvider) WrapExec(
	ctx context.Context,
	workDir string,
	argv, _ []string,
) (*exec.Cmd, error) {
	p.wrappedArg = append(p.wrappedArg, append([]string(nil), argv...))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workDir
	return cmd, nil
}

func (p *recordingProvider) LookPath(_ context.Context, _ string, names []string) (string, error) {
	p.lookedUp = append(p.lookedUp, append([]string(nil), names...))
	if p.path == "" {
		return "", errors.New("executable not found: " + strings.Join(names, ", "))
	}
	return p.path, nil
}

func (p *recordingProvider) Close() error { return nil }

func TestDefaultServersResolveThroughActivatedLookPathAndWrapExec(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project-tool")
	provider := &recordingProvider{path: path}
	m := &manager{servers: defaultServers(), provider: provider}

	for _, server := range m.servers {
		t.Run(server.ID, func(t *testing.T) {
			require.NotEmpty(t, server.PathNames, "built-in server must use activated PATH lookup")
			cmd, err := m.wrappedServerCommand(context.Background(), &server, root)
			require.NoError(t, err)
			require.Len(t, provider.lookedUp, len(provider.wrappedArg))
			assert.Equal(t, server.PathNames, provider.lookedUp[len(provider.lookedUp)-1])
			assert.Equal(t, append([]string{path}, server.Args...), provider.wrappedArg[len(provider.wrappedArg)-1])
			assert.Equal(t, append([]string{path}, server.Args...), cmd.Args)
			if server.ID == "terraform-ls" {
				assert.Equal(t, []string{path, "serve"}, cmd.Args)
			}
		})
	}

	assert.Len(t, provider.lookedUp, len(m.servers))
	assert.Len(t, provider.wrappedArg, len(m.servers))
}

func TestDefaultServersIncludeDockerfileAndPHPResolvers(t *testing.T) {
	servers := defaultServers()
	var dockerfile, php *serverConfig
	for i := range servers {
		switch {
		case containsString(servers[i].Extensions, ".dockerfile"):
			dockerfile = &servers[i]
		case containsString(servers[i].Extensions, ".php"):
			php = &servers[i]
		}
	}

	require.NotNil(t, dockerfile)
	require.NotNil(t, php)
	assert.Contains(t, dockerfile.Extensions, "Dockerfile")
	assert.Equal(t, []string{"docker-langserver"}, dockerfile.PathNames)
	assert.Equal(t, []string{"--stdio"}, dockerfile.Args)
	assert.Equal(t, []string{"intelephense"}, php.PathNames)
	assert.Equal(t, []string{"--stdio"}, php.Args)

	provider := &recordingProvider{path: "/project/bin/lsp"}
	m := &manager{provider: provider}
	for _, server := range []*serverConfig{dockerfile, php} {
		cmd, err := m.wrappedServerCommand(context.Background(), server, t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, []string{"/project/bin/lsp", "--stdio"}, cmd.Args)
	}
}

func TestManagerServerForCanonicalDockerfile(t *testing.T) {
	m := &manager{servers: defaultServers()}

	server := m.serverFor(filepath.Join(t.TempDir(), "Dockerfile"))
	require.NotNil(t, server)
	assert.Equal(t, "dockerfile-ls", server.ID)
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func TestMissingServerDoesNotInstallAndReturnsUnavailableError(t *testing.T) {
	home := t.TempDir()
	provider := &recordingProvider{}
	m := &manager{provider: provider}
	server := &serverConfig{ID: "missing-lsp", PathNames: []string{"missing-lsp"}}

	_, err := m.spawnServer(context.Background(), server, home)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrServerUnavailable)
	assert.Contains(t, err.Error(), "missing-lsp is not on the activated PATH")
	assert.Empty(t, provider.wrappedArg, "an unavailable server must not be wrapped or launched")
	entries, readErr := os.ReadDir(home)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "missing server lookup must not create an install/download tree")
}

func TestProjectLocalExecutableIsUsedWhenOnPath(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0o755))
	executable := filepath.Join(binDir, "project-lsp")
	require.NoError(t, os.WriteFile(executable, []byte("test"), 0o755))
	t.Setenv("PATH", binDir)

	m := &manager{}
	cmd, err := m.spawnServer(
		context.Background(),
		&serverConfig{ID: "project-lsp", PathNames: []string{"project-lsp"}},
		workDir,
	)
	require.NoError(t, err)
	assert.Equal(t, executable, cmd.Args[0])
}
