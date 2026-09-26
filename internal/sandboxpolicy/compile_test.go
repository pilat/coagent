package sandboxpolicy

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolProfiles is a synthetic profile set: compile semantics are asserted
// against it so shipped-catalog edits never rewrite the expectations.
func toolProfiles(t *testing.T, home string) (map[string]Profile, string, string, string) {
	t.Helper()

	basicCache := testDir(t, filepath.Join(home, ".cache", "tool-a"))
	basicState := testDir(t, filepath.Join(home, ".local", "state", "tool-a"))
	escalatedShare := testDir(t, filepath.Join(home, ".local", "share", "tool-a"))

	profiles := map[string]Profile{
		"tool-a": {
			Mounts: []Mount{
				{Path: basicCache, Mode: ModeReadWrite, Type: LevelBasic},
				{Path: basicState, Mode: ModeReadOnly, Type: LevelBasic},
				{Path: escalatedShare, Mode: ModeReadWrite, Type: LevelEscalated},
			},
		},
		"tool-b": {
			Mounts: []Mount{
				{Path: testDir(t, filepath.Join(home, "tool-b-data")), Mode: ModeReadOnly, Type: LevelEscalated},
			},
			Sockets: []Socket{{Path: filepath.Join(home, "tool-b.sock"), Type: LevelEscalated}},
			Network: []Network{
				{Address: HostLoopback, Protocol: ProtocolTCP, Ports: []int{8080}, Type: LevelEscalated},
			},
		},
	}

	return profiles, basicCache, basicState, escalatedShare
}

func TestCompile_ActivatesBasicEntriesByDefault(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	profiles, basicCache, basicState, escalatedShare := toolProfiles(t, home)

	policy, err := Compile(testCatalog(t, profiles), testRequest(t, project))
	require.NoError(t, err)

	cache, ok := grantFor(policy, basicCache)
	require.True(t, ok, "basic read-write cache must be granted")
	assert.Equal(t, ModeReadWrite, cache.Mode)
	assert.Equal(t, LevelBasic, cache.Level)
	assert.Equal(t, "tool-a", cache.Profile)
	assert.True(t, cache.Present)

	state, ok := grantFor(policy, basicState)
	require.True(t, ok, "basic read-only state must be granted")
	assert.Equal(t, ModeReadOnly, state.Mode)

	_, ok = grantFor(policy, escalatedShare)
	assert.False(t, ok, "escalated mount must not apply without a grant")

	_, ok = networkFor(policy, HostLoopback)
	assert.False(t, ok, "escalated network rule must not apply without a grant")
}

func TestPrivateRuntimeTargetsCannotShadowNetworkEntry(t *testing.T) {
	for _, target := range []string{"/run/coagent", "/run/coagent/net-entry", "/run/coagent/other"} {
		t.Run(target, func(t *testing.T) {
			err := validatePrivateRuntimeTargets([]Grant{{Target: target}}, nil)
			require.ErrorContains(t, err, "private sandbox runtime")
		})
	}

	require.NoError(t, validatePrivateRuntimeTargets([]Grant{{Target: "/run/user/1000"}}, nil))
	require.ErrorContains(t,
		validatePrivateRuntimeTargets(nil, []SocketGrant{{Path: "/run/coagent/net-entry"}}),
		"private sandbox runtime",
	)
	for _, path := range []string{"/etc", "/etc/resolv.conf", "/etc/hosts"} {
		require.ErrorContains(t, validateGrantPath(path, false), "sandbox network runtime files")
	}
	require.NoError(t, validatePrivateRuntimeTargets([]Grant{{
		Target: "/etc/resolv.conf", Mode: ModeReadOnly, Kind: KindFile,
	}}, nil), "the fixed read-only execution substrate remains available")
}

func TestCompile_UnionsGlobalAndProjectEscalation(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	profiles, _, _, escalatedShare := toolProfiles(t, home)

	req := testRequest(t, project)
	req.GlobalEscalated = []string{"tool-a"}
	req.ProjectEscalated = []string{"tool-b"}

	policy, err := Compile(testCatalog(t, profiles), req)
	require.NoError(t, err)

	share, ok := grantFor(policy, escalatedShare)
	require.True(t, ok)
	assert.Equal(t, LevelEscalated, share.Level)

	socket, ok := socketFor(policy, filepath.Join(home, "tool-b.sock"))
	require.True(t, ok, "project escalation must add tool-b's socket")
	assert.Equal(t, LevelEscalated, socket.Level)

	network, ok := networkFor(policy, HostLoopback)
	require.True(t, ok, "project escalation must add tool-b's network rule")
	assert.Equal(t, []int{8080}, network.Ports)
}

func TestCompile_ShieldsRemoveEveryProfileEntry(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	profiles, _, _, escalatedShare := toolProfiles(t, home)

	req := testRequest(t, project)
	req.Shields = true
	req.GlobalEscalated = []string{"tool-a", "tool-b"}

	policy, err := Compile(testCatalog(t, profiles), req)
	require.NoError(t, err)

	for _, grant := range policy.Grants {
		assert.Empty(t, grant.Profile, "shields must remove profile grants, found %q", grant.Target)
	}

	assert.Empty(t, policy.Sockets)
	assert.Empty(t, policy.Network)

	base, ok := grantFor(policy, project)
	require.True(t, ok, "base project grant survives shields")
	assert.Equal(t, ModeReadWrite, base.Mode)

	_, ok = grantFor(policy, TempPath)
	assert.True(t, ok, "private temporary storage survives shields")

	_, ok = grantFor(policy, escalatedShare)
	assert.False(t, ok)
}

func TestCompile_GrantsPrivateTemporaryStorage(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)

	policy, err := Compile(testCatalog(t, nil), testRequest(t, project))
	require.NoError(t, err)

	temp, ok := grantFor(policy, TempPath)
	require.True(t, ok)
	assert.Equal(t, policy.TempRoot, temp.Source)
	assert.Equal(t, ModeReadWrite, temp.Mode)

	varTemp, ok := grantFor(policy, VarTempPath)
	require.True(t, ok)
	assert.Equal(t, policy.VarTempRoot, varTemp.Source)
	assert.Contains(t, policy.VarTempRoot, policy.TempRoot)
}

func TestCompile_GrantsGitMetadataInBothShieldStates(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	gitDir := testDir(t, filepath.Join(home, "main-repo", ".git"))

	for _, shields := range []bool{false, true} {
		req := testRequest(t, project)
		req.GitMetadataRoot = gitDir
		req.Shields = shields

		policy, err := Compile(testCatalog(t, nil), req)
		require.NoError(t, err)

		grant, ok := grantFor(policy, gitDir)
		require.True(t, ok, "linked worktree git metadata must be granted (shields=%t)", shields)
		assert.Equal(t, ModeReadWrite, grant.Mode)
		assert.True(t, grant.Present)
	}
}

func TestCompile_TranslatesLegacyWritablePaths(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	legacy := testDir(t, filepath.Join(home, "legacy"))

	req := testRequest(t, project)
	req.LegacyWritable = []string{legacy}

	policy, err := Compile(testCatalog(t, nil), req)
	require.NoError(t, err)

	grant, ok := grantFor(policy, legacy)
	require.True(t, ok)
	assert.Equal(t, ModeReadWrite, grant.Mode)
	assert.Equal(t, legacyProfile, grant.Profile)

	req.Shields = true
	shielded, err := Compile(testCatalog(t, nil), req)
	require.NoError(t, err)

	_, ok = grantFor(shielded, legacy)
	assert.False(t, ok, "shields suppress legacy operator grants")
}

func TestCompile_RejectsUnknownEscalationProfile(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)

	req := testRequest(t, project)
	req.GlobalEscalated = []string{"missing"}

	_, err := Compile(testCatalog(t, nil), req)
	assert.ErrorContains(t, err, `unknown profile "missing"`)
}

func TestCompile_RejectsTempRootOutsideCoagentHome(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)

	req := testRequest(t, project)
	req.TempRoot = filepath.Join(home, "elsewhere")

	_, err := Compile(testCatalog(t, nil), req)
	assert.ErrorContains(t, err, "outside the coagent home")
}

func TestCompile_DigestIsDeterministicAndIgnoresPresence(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	absent := filepath.Join(home, ".cache", "absent-tool")

	profiles := map[string]Profile{
		"tool-c": {Mounts: []Mount{{Path: absent, Mode: ModeReadWrite, Type: LevelBasic}}},
	}
	catalog := testCatalog(t, profiles)

	first, err := Compile(catalog, testRequest(t, project))
	require.NoError(t, err)

	second, err := Compile(catalog, testRequest(t, project))
	require.NoError(t, err)
	assert.Equal(t, first.Digest, second.Digest, "digest must be stable across compiles")

	grant, ok := grantFor(first, absent)
	require.True(t, ok, "an absent declaration is still authorized")
	assert.False(t, grant.Present)

	testDir(t, absent)

	third, err := Compile(catalog, testRequest(t, project))
	require.NoError(t, err)
	assert.Equal(t, first.Digest, third.Digest, "object presence must not change authority identity")

	present, ok := grantFor(third, absent)
	require.True(t, ok)
	assert.True(t, present.Present)
}

func TestCompile_DigestChangesWithShieldsAndEscalation(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	profiles, _, _, _ := toolProfiles(t, home)
	catalog := testCatalog(t, profiles)

	base, err := Compile(catalog, testRequest(t, project))
	require.NoError(t, err)

	req := testRequest(t, project)
	req.Shields = true

	shielded, err := Compile(catalog, req)
	require.NoError(t, err)
	assert.NotEqual(t, base.Digest, shielded.Digest)

	req = testRequest(t, project)
	req.GlobalEscalated = []string{"tool-a"}

	escalated, err := Compile(catalog, req)
	require.NoError(t, err)
	assert.NotEqual(t, base.Digest, escalated.Digest)
}

func TestCompile_ProjectsDoNotShareAuthority(t *testing.T) {
	home := testHome(t)
	first := testProject(t, home)
	second := testDir(t, filepath.Join(home, "project-two"))
	catalog := testCatalog(t, nil)

	firstPolicy, err := Compile(catalog, testRequest(t, first))
	require.NoError(t, err)

	secondReq := Request{
		ProjectRoot: second, ProjectID: 2, WorkDir: second, TempRoot: tempRootFor(t, "project-2"),
	}

	secondPolicy, err := Compile(catalog, secondReq)
	require.NoError(t, err)

	firstTemp, ok := grantFor(firstPolicy, TempPath)
	require.True(t, ok)
	secondTemp, ok := grantFor(secondPolicy, TempPath)
	require.True(t, ok)
	assert.NotEqual(t, firstTemp.Source, secondTemp.Source, "each project owns its own temporary storage")

	_, ok = grantFor(firstPolicy, second)
	assert.False(t, ok, "a project is not granted another project's root")
	assert.NotEqual(t, firstPolicy.Digest, secondPolicy.Digest)
}

func TestCompile_DropsEntryForUnsetEnvironmentName(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	t.Setenv("SSH_AUTH_SOCK", "")

	req := testRequest(t, project)
	req.GlobalEscalated = []string{"ssh"}

	policy, err := Compile(testCatalog(t, nil), req)
	require.NoError(t, err)

	assert.Empty(t, policy.Sockets, "an unset environment name with no default drops its socket entry")
}

func TestCompile_UsesOnlyCapturedEnvironmentForSocketGrant(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	socket := filepath.Join(home, "agent.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	req := testRequest(t, project)
	req.GlobalEscalated = []string{"ssh"}
	req.Environment = map[string]string{"SSH_AUTH_SOCK": socket}
	policy, err := Compile(testCatalog(t, nil), req)
	require.NoError(t, err)
	require.Len(t, policy.Sockets, 1)
	assert.Equal(t, socket, policy.Sockets[0].Path)
	req.Environment = nil
	without, err := Compile(testCatalog(t, nil), req)
	require.NoError(t, err)
	assert.Empty(t, without.Sockets)
}
