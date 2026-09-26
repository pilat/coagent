//go:build integration && linux

package bashsandbox

import (
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/shellenv"
)

func installProfileTool(t *testing.T, name, home, source string) string {
	t.Helper()
	source, err := filepath.EvalSymlinks(source)
	require.NoError(t, err)
	destination := filepath.Join(home, ".local", "bin", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(destination), 0o700))
	if err := os.Link(source, destination); err == nil {
		return destination
	}
	input, err := os.Open(source)
	require.NoError(t, err)
	defer func() { _ = input.Close() }()
	// #nosec G302 -- the copied test tool must remain executable in the sandbox.
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	require.NoError(t, err)
	_, err = io.Copy(output, input)
	require.NoError(t, err)
	require.NoError(t, output.Close())
	return destination
}

func TestProfileSmoke_MisePinnedToolchainInShellAndMCP(t *testing.T) {
	source, err := exec.LookPath("mise")
	require.NoError(t, err)
	home := testSandboxHome(t)
	mise := installProfileTool(t, "mise", home, source)
	t.Setenv("PATH", filepath.Join(home, ".local", "bin")+":/usr/bin:/bin")
	t.Setenv("MISE_OFFLINE", "1")
	t.Setenv("MISE_YES", "1")
	t.Setenv("MISE_NO_PROGRESS", "1")
	project := testDir(t, filepath.Join(home, "project"))
	require.NoError(t, os.WriteFile(filepath.Join(project, "mise.toml"), []byte("[tools]\ngo = \"1.99.0\"\n"), 0o600))
	goTool := filepath.Join(
		testDir(t, filepath.Join(home, ".local", "share", "mise", "installs", "go", "1.99.0", "bin")),
		"go",
	)
	require.NoError(t, os.WriteFile(goTool, []byte("#!/bin/sh\necho 'go version go1.99.0 profile-fixture'\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("source ~/.bashrc\n"), 0o600))
	require.NoError(
		t,
		os.WriteFile(
			filepath.Join(home, ".bashrc"),
			[]byte("eval \"$(\"$HOME/.local/bin/mise\" activate bash)\"\n"),
			0o600,
		),
	)
	server := filepath.Join(project, "mise-mcp")
	mcpOutput := filepath.Join(project, "mcp-go-version")
	require.NoError(t, os.WriteFile(server, []byte(`#!/bin/sh
go version > "$PROFILE_OUTPUT"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"mise-profile","version":"1"}}}\n' "$id" ;;
    *'"method":"tools/list"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}\n' "$id" ;;
  esac
done
`), 0o700))
	policy := testCompiledPolicy(t, project, false)
	provider := shellenv.New()
	defer func() { require.NoError(t, provider.Close()) }()
	runner, err := New(Config{Enabled: true, Policy: policy, WorkDir: project, SessionKey: "mise-profile"}, provider)
	require.NoError(t, err)
	output, err := runSandboxCommand(t, runner, mise+" exec -- go version", project)
	require.NoError(t, err, output)
	assert.Contains(t, output, "go1.99.0 profile-fixture")
	cmd, err := runner.ShellCommand(t.Context(), "go version", project)
	require.NoError(t, err)
	activated, err := cmd.CombinedOutput()
	procexec.CloseExtraFiles(cmd)
	require.NoError(t, err, string(activated))
	assert.Contains(t, string(activated), "go1.99.0 profile-fixture")
	client, err := mcp.NewClient(t.Context(), "mise-profile", mcp.ServerConfig{
		Command: server, WorkDir: project, Env: map[string]string{"PROFILE_OUTPUT": mcpOutput},
	}, provider, runner)
	require.NoError(t, err)
	require.NoError(t, client.Close())
	content, err := os.ReadFile(mcpOutput)
	require.NoError(t, err)
	assert.Contains(t, string(content), "go1.99.0 profile-fixture")
}

func TestProfileSmoke_GhFileCredentialEscalation(t *testing.T) {
	// The CI PATH may contain a mise shim, which cannot run in the fixture HOME.
	resolved, err := exec.Command("mise", "which", "gh").Output()
	require.NoError(t, err)
	home := testSandboxHome(t)
	gh := installProfileTool(t, "gh", home, strings.TrimSpace(string(resolved)))
	project := testDir(t, filepath.Join(home, "project"))
	var authenticated atomic.Int32
	var unauthenticated atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v3/user", r.URL.Path)
		if !strings.Contains(r.Header.Get("Authorization"), "fixture-token") {
			unauthenticated.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"login":"profile-fixture"}`)
	}))
	defer api.Close()
	host := strings.TrimPrefix(api.URL, "https://")
	// gh uses the URL hostname, without its port, to select a stored token.
	hostname, _, err := net.SplitHostPort(host)
	require.NoError(t, err)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	caPath := filepath.Join(project, "ca.pem")
	require.NoError(t, os.WriteFile(caPath, cert, 0o600))
	configDir := testDir(t, filepath.Join(home, ".config", "gh"))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yml"), []byte("git_protocol: https\n"), 0o600))
	require.NoError(
		t,
		os.WriteFile(
			filepath.Join(configDir, "hosts.yml"),
			[]byte(fmt.Sprintf(
				"%s:\n  user: fixture\n  oauth_token: fixture-token\n  git_protocol: https\n",
				hostname,
			)),
			0o600,
		),
	)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "unrelated-secret"), []byte("hidden"), 0o600))
	t.Setenv("GH_HOST", host)
	t.Setenv("SSL_CERT_FILE", caPath)
	t.Setenv("GH_PROMPT_DISABLED", "1")
	t.Setenv("GH_NO_UPDATE_NOTIFIER", "1")
	t.Setenv("NO_PROXY", "*")
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(name, "")
	}
	basic := testRunnerFromPolicy(t, testCompiledPolicy(t, project, false), project, "gh-basic")
	output, err := runSandboxCommand(t, basic, gh+" --version", project)
	require.NoError(t, err, output)
	output, err = runSandboxCommand(t, basic, gh+" api user --jq .login", project)
	require.Error(t, err, "basic gh cannot read its file credential: %s", output)
	assert.Equal(t, int32(0), authenticated.Load())
	assert.Equal(t, int32(1), unauthenticated.Load())
	policy := compileFixturePolicy(t, project, false, nil, func(req *sandboxpolicy.Request) {
		req.GlobalEscalated = []string{"gh"}
	})
	escalated := testRunnerFromPolicy(t, policy, project, "gh-escalated")
	output, err = runSandboxCommand(t, escalated, gh+" api user --jq .login", project)
	require.NoError(t, err, output)
	assert.Equal(t, "profile-fixture", strings.TrimSpace(output))
	assert.Equal(t, int32(1), authenticated.Load())
	output, err = runSandboxCommand(
		t,
		escalated,
		"test ! -e "+shellQuote(filepath.Join(configDir, "unrelated-secret")),
		project,
	)
	require.NoError(t, err, output)
	shielded := testRunnerFromPolicy(t, testCompiledPolicy(t, project, true), project, "gh-shielded")
	_, err = runSandboxCommand(t, shielded, gh+" --version", project)
	require.Error(t, err)
}
