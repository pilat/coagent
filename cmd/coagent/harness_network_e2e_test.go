//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxnet"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// The compiled binary builds the real namespace, joins it through Bubblewrap,
// forwards an admitted host-loopback service and keeps a guest-local server
// separate from the daemon host's service on the same port.
func TestHarnessE2E_NetworkJoinAndWebLoopback(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Fatal(err)
	}
	binary := buildBinary(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "project")
	require.NoError(t, os.Mkdir(project, 0o700))
	serverScript := `import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class Handler(BaseHTTPRequestHandler):
 def do_GET(self):
  body = b'{"results":[{"title":"guest-search","url":"https://example.com","content":"guest-only"}]}' if self.path.startswith('/search') else b'guest-only'
  self.send_response(200)
  self.send_header('Content-Type', 'application/json' if self.path.startswith('/search') else 'text/plain')
  self.end_headers()
  self.wfile.write(body)
 def log_message(self, *args): pass
server = HTTPServer(('127.0.0.1', int(sys.argv[1])), Handler)
print('ready', file=sys.stderr, flush=True)
server.serve_forever()
`
	require.NoError(t, os.WriteFile(filepath.Join(project, "server.py"), []byte(serverScript), 0o600))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	hostServer := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "host-only") },
		),
	}
	go func() { _ = hostServer.Serve(listener) }()
	t.Cleanup(func() { _ = hostServer.Close() })
	profiles := map[string]sandboxpolicy.Profile{
		"test-host": {Network: []sandboxpolicy.Network{{
			Address: sandboxpolicy.HostLoopback, Protocol: sandboxpolicy.ProtocolTCP,
			Ports: []int{port}, Type: sandboxpolicy.LevelBasic,
		}}},
	}
	catalog, err := sandboxpolicy.Load(profiles)
	require.NoError(t, err)
	substrate, err := bashsandbox.ExecutionSubstrate()
	require.NoError(t, err)
	tempRoot, err := coagenthome.SandboxTempDir(coagenthome.SandboxPathIdentity(project))
	require.NoError(t, err)
	policy, err := sandboxpolicy.Compile(catalog, sandboxpolicy.Request{
		ProjectRoot: project, WorkDir: project, TempRoot: tempRoot, Substrate: substrate,
	})
	require.NoError(t, err)
	owner := newNetworkOwner(t.Context())
	owner.start = func(ctx context.Context, rootID int64, policy sandboxpolicy.Policy) (*networkGeneration, error) {
		return startNetworkGenerationWithBinary(ctx, rootID, policy, binary)
	}
	t.Cleanup(func() { _ = owner.Stop(context.Background()) })
	lease, err := owner.Acquire(t.Context(), 1, policy)
	if errors.Is(err, sandboxnet.ErrSetupUnsupported) && os.Getenv("CI") == "" {
		t.Skip(err)
	}
	require.NoError(t, err)
	defer lease.Release()
	runner, err := bashsandbox.New(bashsandbox.Config{
		Enabled: true, Policy: policy, WorkDir: project, SessionKey: "network-e2e", Network: lease.Link(),
	}, nil)
	require.NoError(t, err)
	lease.BindRunner(runner, project)

	aliasURL := fmt.Sprintf("http://%s:%d/", sandboxnet.HostAlias, port)
	cmd, err := runner.BashCommand(t.Context(), "curl --noproxy '*' -fsS "+aliasURL, project)
	require.NoError(t, err)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.Equal(t, "host-only", string(output))

	guestPort := strconv.Itoa(port)
	cmd, err = runner.Command(t.Context(), procexec.Request{
		Path: "/usr/bin/python3", Args: []string{"server.py", guestPort}, WorkDir: project,
	})
	require.NoError(t, err)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	for _, file := range cmd.ExtraFiles {
		_ = file.Close()
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stderr).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		require.Contains(t, line, "ready")
	case <-time.After(10 * time.Second):
		t.Fatal("guest server did not become ready")
	}
	lease.Release()
	owner.reapIdle(t.Context(), time.Now().Add(11*time.Minute))
	assert.NotNil(t, owner.entries[1], "a detached guest server holds the generation")
	transport := &http.Transport{DialContext: lease.DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://localhost:%d/", port))
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, "guest-only", string(body))

	unified := &config.UnifiedConfig{
		Sandbox: sandboxpolicy.Section{Enabled: true, Profiles: profiles},
		Tools: config.ToolsConfig{Search: config.SearchToolConfig{
			Provider: config.SearchProviderSearxng,
			BaseURL:  fmt.Sprintf("http://localhost:%d", port),
		}},
	}
	stack, err := builtin.BuildStack(t.Context(), builtin.StackConfig{
		SessionID: 1, WorkDir: project, Unified: unified, NetworkOwner: owner,
		Loader: loader.New(), Todo: todo.New(),
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, stack.Close()) }()
	fetchParams, err := json.Marshal(map[string]string{"url": fmt.Sprintf("http://localhost:%d/", port)})
	require.NoError(t, err)
	fetched, err := stack.Registry.Get("webfetch").Execute(t.Context(), fetchParams)
	require.NoError(t, err)
	assert.Equal(t, "guest-only", fetched.Output)
	searchParams, err := json.Marshal(map[string]string{"query": "guest"})
	require.NoError(t, err)
	searched, err := stack.Registry.Get("websearch").Execute(t.Context(), searchParams)
	require.NoError(t, err)
	assert.Contains(t, searched.Output, "guest-search")
}
