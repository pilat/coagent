package bashsandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func TestProcessPolicyExecutableLookupUsesGrantedPaths(t *testing.T) {
	project := t.TempDir()
	external := t.TempDir()
	externalTool := filepath.Join(external, "external-tool")
	require.NoError(t, os.WriteFile(externalTool, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	projectTool := filepath.Join(project, "project-tool")
	require.NoError(t, os.WriteFile(projectTool, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", external+string(os.PathListSeparator)+project)

	req := testPolicyRequest(t, project)
	req.GlobalRules = []sandboxpolicy.Rule{{Deny: external}}
	compiled, err := sandboxpolicy.Compile(req)
	require.NoError(t, err)
	policy := testProcessPolicy(t, compiled)

	// The host root is readable by default: only an explicit deny withholds a
	// path PATH happens to resolve.
	_, err = policy.resolveExecutable("external-tool", project)
	require.ErrorContains(t, err, "outside the sandbox's granted paths")
	resolved, err := policy.resolveExecutable("project-tool", project)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(policy.projectRoot, "project-tool"), resolved)
	resolved, err = policy.resolveExecutable("./project-tool", project)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(policy.projectRoot, "project-tool"), resolved)
}

func TestProcessPolicyValidateWorkDirStaysInsideProject(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	require.NoError(t, os.Mkdir(project, 0o700))

	policy := testProcessPolicy(t, testCompiledPolicy(t, project))

	require.NoError(t, policy.validateWorkDir(project))
	require.ErrorContains(t, policy.validateWorkDir(""), "outside the sandboxed project")
	assert.ErrorContains(t, policy.validateWorkDir(base), "outside the sandboxed project")
}

func TestProcessPolicyKeyIncludesSessionIdentity(t *testing.T) {
	project := t.TempDir()

	first := testProcessPolicy(t, testCompiledPolicy(t, project))
	other := testProcessPolicy(t, testCompiledPolicy(t, project), "session:2")

	assert.NotEqual(t, first.key(), other.key(), "session identity is part of the policy key")
	assert.Equal(t, policyKey(first.policy.Digest, first.sessionKey), first.key())
}

func TestProcessPolicyReadScopeReflectsPolicyEntries(t *testing.T) {
	project := t.TempDir()

	disabled, err := buildProcessPolicy(Config{WorkDir: project, SessionKey: "session:1"})
	require.NoError(t, err)
	assert.Equal(t, HostReadable, disabled.readScope())

	enabled := testProcessPolicy(t, testCompiledPolicy(t, project))
	assert.Equal(t, ProjectConfined, enabled.readScope())
}

func TestProcessPolicyHonoursDenyRules(t *testing.T) {
	project := t.TempDir()
	secretDir := t.TempDir()

	req := testPolicyRequest(t, project)
	req.GlobalRules = []sandboxpolicy.Rule{{Deny: secretDir}}

	compiled, err := sandboxpolicy.Compile(req)
	require.NoError(t, err)

	policy := testProcessPolicy(t, compiled)
	assert.True(t, policy.allowsRead(filepath.Join(project, "note")))
	assert.False(t, policy.allowsRead(filepath.Join(secretDir, "id_ed25519")))
}

func TestWritableRootsOfPutsProjectFirst(t *testing.T) {
	project := t.TempDir()
	cache := t.TempDir()

	req := testPolicyRequest(t, project)
	req.GlobalRules = []sandboxpolicy.Rule{{Allow: cache, Mode: sandboxpolicy.ModeReadWrite}}

	compiled, err := sandboxpolicy.Compile(req)
	require.NoError(t, err)

	policy := testProcessPolicy(t, compiled)
	roots := policy.writableRoots
	require.NotEmpty(t, roots)
	assert.Equal(t, policy.projectRoot, roots[0])
	assert.Contains(t, roots, cache)

	runnerRoots := writableRootsOf(compiled)
	require.NotEmpty(t, runnerRoots)
	assert.Equal(t, policy.projectRoot, runnerRoots[0])
}

func TestLauncherUsesEffectivePermissionsIncludingParents(t *testing.T) {
	policy := sandboxpolicy.Policy{Entries: []sandboxpolicy.Entry{
		{Path: "/", Action: sandboxpolicy.ActionAllow, Mode: sandboxpolicy.ModeReadOnly},
		{Path: "/usr", Action: sandboxpolicy.ActionAllow, Mode: sandboxpolicy.ModeReadWrite},
		{Path: "/usr", Action: sandboxpolicy.ActionAllow, Mode: sandboxpolicy.ModeReadOnly},
	}}
	assert.Empty(t, writableLauncherPaths(policy, "/usr/bin/bwrap"))
	assert.Empty(t, policy.WritableRoots())
	policy.Entries = append(
		policy.Entries,
		sandboxpolicy.Entry{Path: "/usr/bin", Action: sandboxpolicy.ActionAllow, Mode: sandboxpolicy.ModeReadWrite},
		sandboxpolicy.Entry{
			Path:   "/usr/bin/bwrap",
			Action: sandboxpolicy.ActionAllow,
			Mode:   sandboxpolicy.ModeReadOnly,
		},
	)
	assert.Equal(t, []string{"/usr/bin"}, writableLauncherPaths(policy, "/usr/bin/bwrap"))
}

func TestBuildProcessPolicyRejectsPolicyWithoutProjectRoot(t *testing.T) {
	_, err := buildProcessPolicy(Config{
		Enabled: true, WorkDir: t.TempDir(), SessionKey: "session:1",
		Policy: sandboxpolicy.Policy{Entries: []sandboxpolicy.Entry{{
			Path: "/srv/data", Action: sandboxpolicy.ActionAllow, Mode: sandboxpolicy.ModeReadOnly,
			Kind: sandboxpolicy.KindDir, Present: true,
		}}},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "no project root")
}

func TestBuildProcessPolicyRejectsEnabledSandboxWithoutCompiledPolicy(t *testing.T) {
	_, err := buildProcessPolicy(Config{Enabled: true, WorkDir: t.TempDir(), SessionKey: "session:1"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "without a compiled policy")
}

func TestInnerEnvironmentRewritesPWD(t *testing.T) {
	workDir := "/work dir"

	tests := map[string]struct {
		env  []string
		want []environmentEntry
	}{
		"nil falls back to the process environment and keeps names": {
			want: []environmentEntry{{name: "COAGENT_TEST_SENTINEL", value: "kept"}},
		},
		"replaces PWD": {
			env: []string{"PWD=/somewhere/else", "HOME=/home/user"},
			want: []environmentEntry{
				{name: "PWD", value: workDir},
				{name: "HOME", value: "/home/user"},
			},
		},
		"adds PWD when absent": {
			env:  []string{"PATH=/usr/bin:/bin"},
			want: []environmentEntry{{name: "PATH", value: "/usr/bin:/bin"}, {name: "PWD", value: workDir}},
		},
	}

	t.Setenv("COAGENT_TEST_SENTINEL", "kept")
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			entries, err := innerEnvironment(tt.env, workDir)
			require.NoError(t, err)

			if tt.env == nil {
				for _, want := range tt.want {
					assert.Contains(t, entries, want)
				}

				return
			}

			assert.ElementsMatch(t, tt.want, entries)
		})
	}
}

func TestInnerEnvironmentRejectsInvalidEntry(t *testing.T) {
	tests := map[string]struct {
		env     []string
		message string
	}{
		"no separator":   {env: []string{"BROKEN"}, message: "invalid process environment entry"},
		"empty name":     {env: []string{"=value"}, message: "invalid process environment entry"},
		"nul in name":    {env: []string{"NA\x00ME=value"}, message: "invalid process environment entry"},
		"nul in value":   {env: []string{"NAME=va\x00lue"}, message: "invalid process environment entry"},
		"empty is valid": {env: []string{"NAME="}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			entries, err := innerEnvironment(tt.env, "/work")
			if tt.message == "" {
				require.NoError(t, err)
				assert.Equal(t, environmentEntry{name: "NAME", value: ""}, entries[0])

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

func TestSourceLineQuotesSnapshotPath(t *testing.T) {
	assert.Equal(t, "source '/tmp/snap dir/s'; go version", sourceLine("/tmp/snap dir/s", "go version"))
	assert.Equal(t, "source '/a/'\\''b'; exit", sourceLine("/a/'b", "exit"))
}

func TestPolicyKeyIsStableAndInputSensitive(t *testing.T) {
	first := policyKey("digest-a", "session:1")
	assert.Equal(t, first, policyKey("digest-a", "session:1"))
	assert.NotEqual(t, first, policyKey("digest-b", "session:1"))
	assert.NotEqual(t, first, policyKey("digest-a", "session:2"))
	assert.Len(t, first, 64)
}

func TestLimitedBufferBoundsOutput(t *testing.T) {
	var buffer limitedBuffer
	payload := make([]byte, preflightOutputLimit+1024)
	for i := range payload {
		payload[i] = 'x'
	}

	written, err := buffer.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, len(payload), written, "the writer sees its full write")
	assert.Len(t, buffer.String(), preflightOutputLimit)

	_, err = buffer.Write([]byte("more"))
	require.NoError(t, err)
	assert.Len(t, buffer.String(), preflightOutputLimit)
}

func TestSandboxLauncherEnvironmentIsMinimal(t *testing.T) {
	assert.Equal(t, []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}, sandboxLauncherEnvironment())
}

func TestPathWithinRoot(t *testing.T) {
	tests := map[string]struct {
		path string
		root string
		want bool
	}{
		"root itself":     {path: "/project", root: "/project", want: true},
		"inside":          {path: "/project/sub/file", root: "/project", want: true},
		"sibling":         {path: "/project-other/file", root: "/project", want: false},
		"parent":          {path: "/project", root: "/project/sub", want: false},
		"unrelated":       {path: "/elsewhere/file", root: "/project", want: false},
		"filesystem root": {path: "/anything", root: "/", want: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, pathWithinRoot(tt.path, tt.root))
		})
	}
}

func TestNewRejectsEnabledSandboxWithoutCompiledPolicy(t *testing.T) {
	// An enabled config cannot even be built without a compiled policy: New
	// fails before touching any platform backend.
	_, err := New(Config{Enabled: true, WorkDir: t.TempDir()}, nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "without a compiled policy")
}

func TestProbeEnforcementRejectsBackendThatRunsNothing(t *testing.T) {
	err := probeEnforcement(func(processPolicy) (Runner, error) {
		return noopRunner{}, nil
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "verify allowed probe")
}

func TestProbeEnforcementRejectsBackendThatDoesNotConfine(t *testing.T) {
	err := probeEnforcement(func(processPolicy) (Runner, error) {
		return disabledRunner{}, nil
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "sandbox allowed write to denied probe path")
}

func TestProbeEnforcementPropagatesBackendConstructionError(t *testing.T) {
	want := errors.New("no backend")

	err := probeEnforcement(func(processPolicy) (Runner, error) {
		return nil, want
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}
