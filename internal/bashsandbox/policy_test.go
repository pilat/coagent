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

	// PATH lookup grants no authority: only the policy's granted targets do.
	policy, err := buildProcessPolicy(Config{
		Enabled: true,
		Policy: sandboxpolicy.Policy{
			ProjectRoot: project,
			Grants: []sandboxpolicy.Grant{{
				Source: project, Target: project,
				Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir, Present: true,
			}},
		},
		WorkDir: project, SessionKey: "session:1",
	})
	require.NoError(t, err)

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
	testSandboxHome(t)

	base := t.TempDir()
	project := filepath.Join(base, "project")
	require.NoError(t, os.Mkdir(project, 0o700))

	policy := testProcessPolicy(t, testCompiledPolicy(t, project, false))

	require.NoError(t, policy.validateWorkDir(project))
	require.ErrorContains(t, policy.validateWorkDir(""), "outside the sandboxed project")
	assert.ErrorContains(t, policy.validateWorkDir(base), "outside the sandboxed project")
}

func TestProcessPolicyKeyIncludesSessionAndShields(t *testing.T) {
	testSandboxHome(t)

	project := t.TempDir()

	ordinary := testProcessPolicy(t, testCompiledPolicy(t, project, false))
	shielded := testProcessPolicy(t, testCompiledPolicy(t, project, true))
	other := testProcessPolicy(t, testCompiledPolicy(t, project, false), "session:2")

	assert.NotEqual(t, ordinary.key(), shielded.key(), "shield state is part of the policy digest")
	assert.NotEqual(t, shielded.key(), other.key(), "session identity is part of the policy key")
	assert.NotEqual(t, ordinary.key(), other.key())
	assert.Equal(t, policyKey(ordinary.digest, ordinary.sessionKey), ordinary.key())
}

func TestProcessPolicyShieldsDropProfileGrants(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	grantDir := testDir(t, filepath.Join(home, "tool-cache"))

	catalog, err := sandboxpolicy.Load(map[string]sandboxpolicy.Profile{
		"fixture": {Mounts: []sandboxpolicy.Mount{{
			Path: grantDir, Mode: sandboxpolicy.ModeReadWrite, Type: sandboxpolicy.LevelBasic,
		}}},
	})
	require.NoError(t, err)

	ordinary, err := sandboxpolicy.Compile(catalog, testPolicyRequest(t, project))
	require.NoError(t, err)
	shielded, err := sandboxpolicy.Compile(catalog, testShieldedPolicyRequest(t, project))
	require.NoError(t, err)

	assert.True(t, containsGrantTarget(ordinary.Grants, grantDir))
	assert.False(t, containsGrantTarget(shielded.Grants, grantDir))

	ordinaryPolicy := testProcessPolicy(t, ordinary)
	shieldedPolicy := testProcessPolicy(t, shielded)
	assert.Equal(t, ProjectConfined, ordinaryPolicy.readScope, "every sandbox-enabled session is allowlisted")
	assert.Equal(t, ProjectConfined, shieldedPolicy.readScope)
	assert.Contains(t, ordinaryPolicy.writableRoots, grantDir)
	assert.NotContains(t, shieldedPolicy.writableRoots, grantDir)
}

func TestWritableRootsOfPutsProjectFirst(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	cache := testDir(t, filepath.Join(home, "tool-cache"))

	catalog, err := sandboxpolicy.Load(map[string]sandboxpolicy.Profile{
		"fixture": {Mounts: []sandboxpolicy.Mount{{
			Path: cache, Mode: sandboxpolicy.ModeReadWrite, Type: sandboxpolicy.LevelBasic,
		}}},
	})
	require.NoError(t, err)

	compiled, err := sandboxpolicy.Compile(catalog, testPolicyRequest(t, project))
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

func TestBuildProcessPolicyRejectsPolicyWithoutProjectRoot(t *testing.T) {
	_, err := buildProcessPolicy(Config{
		Enabled: true, WorkDir: t.TempDir(), SessionKey: "session:1",
		Policy: sandboxpolicy.Policy{Grants: []sandboxpolicy.Grant{{
			Source: "/srv/data", Target: "/srv/data", Mode: sandboxpolicy.ModeReadOnly,
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

func TestInnerEnvironmentRewritesTemporaryPathsAndPWD(t *testing.T) {
	workDir := "/work dir"

	tests := map[string]struct {
		env  []string
		want []environmentEntry
	}{
		"nil falls back to the process environment and keeps names": {
			want: []environmentEntry{{name: "COAGENT_TEST_SENTINEL", value: "kept"}},
		},
		"rewrites temp names and replaces PWD": {
			env: []string{
				"TMPDIR=/host/tmp", "TMP=/host/tmp2", "TEMP=/host/tmp3",
				"PWD=/somewhere/else", "HOME=/home/user",
			},
			want: []environmentEntry{
				{name: "TMPDIR", value: sandboxpolicy.TempPath},
				{name: "TMP", value: sandboxpolicy.TempPath},
				{name: "TEMP", value: sandboxpolicy.TempPath},
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

func TestSnapshotRuntimePathAndGrant(t *testing.T) {
	first := snapshotRuntimePath("/cache/snap-a")
	second := snapshotRuntimePath("/cache/snap-b")

	assert.True(t, filepath.IsAbs(first))
	assert.NotEqual(t, first, second)
	assert.True(t, pathWithinRoot(first, snapshotRuntimeDir))
	assert.Equal(t, first, snapshotRuntimePath("/cache/snap-a"), "runtime path is stable per snapshot")

	grants := snapshotGrant("/cache/snap-a")
	require.Len(t, grants, 1)
	assert.Equal(t, "/cache/snap-a", grants[0].Source)
	assert.Equal(t, first, grants[0].Target)
	assert.Equal(t, sandboxpolicy.ModeReadOnly, grants[0].Mode)
	assert.Equal(t, sandboxpolicy.KindFile, grants[0].Kind)
	assert.True(t, grants[0].Present)
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
	testSandboxHome(t)

	err := probeEnforcement(func(processPolicy) (Runner, error) {
		return noopRunner{}, nil
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "verify allowed probe")
}

func TestProbeEnforcementRejectsBackendThatDoesNotConfine(t *testing.T) {
	testSandboxHome(t)

	err := probeEnforcement(func(processPolicy) (Runner, error) {
		return disabledRunner{}, nil
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "sandbox allowed write to denied probe path")
}

func TestProbeEnforcementPropagatesBackendConstructionError(t *testing.T) {
	testSandboxHome(t)

	want := errors.New("no backend")

	err := probeEnforcement(func(processPolicy) (Runner, error) {
		return nil, want
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

func TestProbeShieldEnforcementRejectsBackendThatReadsEverything(t *testing.T) {
	testSandboxHome(t)

	err := probeShieldEnforcement(func(processPolicy) (Runner, error) {
		return disabledRunner{}, nil
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "shielded probe read denied path")
}

func TestProbeShieldEnforcementPropagatesProjectAccessFailure(t *testing.T) {
	testSandboxHome(t)

	err := probeShieldEnforcement(func(processPolicy) (Runner, error) {
		return errorRunner{err: errors.New("no command")}, nil
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "shielded project access probe")
}

func TestProbeShieldEnforcementPropagatesBackendConstructionError(t *testing.T) {
	testSandboxHome(t)

	want := errors.New("no shielded backend")

	err := probeShieldEnforcement(func(processPolicy) (Runner, error) {
		return nil, want
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

func containsGrantTarget(grants []sandboxpolicy.Grant, target string) bool {
	for _, grant := range grants {
		if grant.Target == target {
			return true
		}
	}

	return false
}
