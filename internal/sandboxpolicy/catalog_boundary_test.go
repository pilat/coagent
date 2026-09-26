package sandboxpolicy

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boundaryFixture creates the files and directories the shipped catalog names,
// so presence can be asserted instead of assumed.
func boundaryFixture(t *testing.T, home string) map[string]string {
	t.Helper()

	paths := map[string]string{
		"bashrc":        testFile(t, filepath.Join(home, ".bashrc")),
		"profile":       testFile(t, filepath.Join(home, ".profile")),
		"miseBin":       testFile(t, filepath.Join(home, ".local", "bin", "mise")),
		"miseInstalls":  testDir(t, filepath.Join(home, ".local", "share", "mise", "installs")),
		"miseConfig":    testDir(t, filepath.Join(home, ".config", "mise")),
		"miseCache":     testDir(t, filepath.Join(home, ".cache", "mise")),
		"miseState":     testDir(t, filepath.Join(home, ".local", "state", "mise")),
		"gitConfig":     testFile(t, filepath.Join(home, ".gitconfig")),
		"gitLFS":        testDir(t, filepath.Join(home, ".cache", "git-lfs")),
		"sshConfig":     testFile(t, filepath.Join(home, ".ssh", "config")),
		"knownHosts":    testFile(t, filepath.Join(home, ".ssh", "known_hosts")),
		"ghConfig":      testFile(t, filepath.Join(home, ".config", "gh", "config.yml")),
		"ghBin":         testFile(t, filepath.Join(home, ".local", "bin", "gh")),
		"ghCache":       testDir(t, filepath.Join(home, ".cache", "gh")),
		"miseData":      testDir(t, filepath.Join(home, ".local", "share", "mise")),
		"gitCredential": testFile(t, filepath.Join(home, ".git-credentials")),
		"ghHosts":       testFile(t, filepath.Join(home, ".config", "gh", "hosts.yml")),
		"homeSecret":    testFile(t, filepath.Join(home, "unrelated-secret")),
		"playwright":    testDir(t, filepath.Join(home, ".cache", "ms-playwright")),
		"ccache":        testDir(t, filepath.Join(home, ".cache", "ccache")),
		"conanCache":    testDir(t, filepath.Join(home, ".conan2", "p")),
		"composerCache": testDir(t, filepath.Join(home, ".cache", "composer")),
		"pubCache":      testDir(t, filepath.Join(home, ".pub-cache")),
		"androidSDK":    testDir(t, filepath.Join(home, "Android", "Sdk")),
		"adbKey":        testFile(t, filepath.Join(home, ".android", "adbkey")),
		"denoCache":     testDir(t, filepath.Join(home, ".cache", "deno")),
		"poetryConfig":  testFile(t, filepath.Join(home, ".config", "pypoetry", "config.toml")),
		"condaPkgs":     testDir(t, filepath.Join(home, ".conda", "pkgs")),
	}

	return paths
}

// TestShippedCatalog_CombinedBasicBoundary composes every shipped profile at
// once: the basic union must be usable, every escalated entry must stay out,
// and no unrelated host path may ride along.
func TestShippedCatalog_CombinedBasicBoundary(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	paths := boundaryFixture(t, home)

	catalog := testCatalog(t, nil)
	policy, err := Compile(catalog, testRequest(t, project))
	require.NoError(t, err)

	basic := []string{
		paths["bashrc"], paths["profile"],
		paths["miseBin"], paths["miseInstalls"], paths["miseConfig"], paths["miseCache"], paths["miseState"],
		paths["gitConfig"], paths["gitLFS"],
		paths["sshConfig"], paths["knownHosts"],
		paths["ghBin"], paths["ghConfig"], paths["ghCache"],
	}
	for _, path := range basic {
		grant, ok := grantFor(policy, path)
		require.True(t, ok, "basic entry %q must be active without any escalation", path)
		assert.True(t, grant.Present, "fixture for %q must exist", path)
	}

	escalated := []string{paths["miseData"], paths["gitCredential"], paths["ghHosts"]}
	for _, path := range escalated {
		_, ok := grantFor(policy, path)
		assert.False(t, ok, "escalated entry %q must stay out of the default policy", path)
	}

	_, ok := grantFor(policy, paths["homeSecret"])
	assert.False(t, ok, "an unrelated home file is never granted")

	_, ok = grantFor(policy, filepath.Join(home, "another-project"))
	assert.False(t, ok, "another project's root is never granted")

	// Mode is asserted on the grant itself: a fixture home under /tmp is inside
	// the private-temp target, so a target-path predicate would be meaningless.
	installs, ok := grantFor(policy, paths["miseInstalls"])
	require.True(t, ok)
	assert.Equal(t, ModeReadOnly, installs.Mode, "installed toolchains are read-only")

	cache, ok := grantFor(policy, paths["miseCache"])
	require.True(t, ok)
	assert.Equal(t, ModeReadWrite, cache.Mode, "a shared tool cache is the accepted basic write")
}

// TestShippedCatalog_EscalationAddsOnlyThatProfilesEntries proves a grant never
// widens beyond the profile the operator named.
func TestShippedCatalog_EscalationAddsOnlyThatProfilesEntries(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	paths := boundaryFixture(t, home)

	req := testRequest(t, project)
	req.GlobalEscalated = []string{"git"}

	policy, err := Compile(testCatalog(t, nil), req)
	require.NoError(t, err)

	grant, ok := grantFor(policy, paths["gitCredential"])
	require.True(t, ok, "git escalation admits the git credential file")
	assert.Equal(t, LevelEscalated, grant.Level)

	_, ok = grantFor(policy, paths["ghHosts"])
	assert.False(t, ok, "git escalation must not admit another profile's credentials")

	_, ok = grantFor(policy, paths["miseData"])
	assert.False(t, ok, "git escalation must not admit mise's shared installation")
}

func TestShippedCatalog_MiseEscalationCanUpdateInstalledTool(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	paths := boundaryFixture(t, home)
	catalog := testCatalog(t, nil)
	basic, err := Compile(catalog, testRequest(t, project))
	require.NoError(t, err)
	installGrant, ok := grantFor(basic, paths["miseInstalls"])
	require.True(t, ok)
	assert.Equal(t, ModeReadOnly, installGrant.Mode)
	req := testRequest(t, project)
	req.GlobalEscalated = []string{"mise"}
	escalated, err := Compile(catalog, req)
	require.NoError(t, err)
	dataGrant, ok := grantFor(escalated, paths["miseData"])
	require.True(t, ok)
	assert.Equal(t, ModeReadWrite, dataGrant.Mode)
	installGrant, ok = grantFor(escalated, paths["miseInstalls"])
	if ok {
		assert.Equal(t, ModeReadWrite, installGrant.Mode)
	}
	_, ok = grantFor(escalated, paths["ghHosts"])
	assert.False(t, ok, "mise escalation cannot admit gh credentials")
}

func TestShippedCatalog_MiseIndependentInstallsDirEscalation(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	installs := testDir(t, filepath.Join(home, "custom-installs"))
	boundaryFixture(t, home)

	catalog := testCatalog(t, nil)
	request := testRequest(t, project)
	request.Environment = map[string]string{"MISE_INSTALLS_DIR": installs}
	basic, err := Compile(catalog, request)
	require.NoError(t, err)
	grant, ok := grantFor(basic, installs)
	require.True(t, ok)
	assert.Equal(t, ModeReadOnly, grant.Mode)

	request.GlobalEscalated = []string{"mise"}
	escalated, err := Compile(catalog, request)
	require.NoError(t, err)
	grant, ok = grantFor(escalated, installs)
	require.True(t, ok)
	assert.Equal(t, ModeReadWrite, grant.Mode)
}

// TestShippedCatalog_ShieldsKeepOnlyTheBaseGrants is the shield half of the
// combined boundary: no profile entry survives, the base grants do.
func TestShippedCatalog_ShieldsKeepOnlyTheBaseGrants(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	paths := boundaryFixture(t, home)

	req := testRequest(t, project)
	req.Shields = true
	req.GlobalEscalated = []string{"git", "gh", "ssh", "mise"}

	policy, err := Compile(testCatalog(t, nil), req)
	require.NoError(t, err)

	for _, path := range []string{
		paths["bashrc"], paths["miseInstalls"], paths["miseCache"], paths["gitConfig"],
		paths["gitCredential"], paths["ghConfig"], paths["ghHosts"], paths["knownHosts"],
	} {
		_, ok := grantFor(policy, path)
		assert.False(t, ok, "raised shields remove %q", path)
	}

	assert.True(t, policy.AllowsWrite(project), "the project stays writable")
	assert.True(t, policy.AllowsWrite(TempPath), "private temporary storage stays writable")
	assert.Empty(t, policy.Sockets, "socket exceptions are profile entries")
}

func TestShippedCatalog_DormantProfilesRequireExplicitEscalation(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	npmCache := testDir(t, filepath.Join(home, ".npm"))
	pnpmCLI := testFile(t, filepath.Join(home, ".local", "share", "pnpm", "pnpm"))
	kubeConfig := testFile(t, filepath.Join(home, ".kube", "config"))
	gradleDaemon := testDir(t, filepath.Join(home, ".gradle", "daemon"))
	rustupBin := testDir(t, filepath.Join(home, ".cargo", "bin"))
	goConfig := testDir(t, filepath.Join(home, ".config", "go"))
	helmConfig := testDir(t, filepath.Join(home, ".config", "helm"))
	composerAuth := testFile(t, filepath.Join(home, ".config", "composer", "auth.json"))
	adbKey := testFile(t, filepath.Join(home, ".android", "adbkey"))
	denoCLI := testFile(t, filepath.Join(home, ".deno", "bin", "deno"))
	bunCLI := testFile(t, filepath.Join(home, ".bun", "bin", "bun"))
	podmanStorage := testDir(t, filepath.Join(home, ".local", "share", "containers", "storage"))
	awsCredentials := testFile(t, filepath.Join(home, ".aws", "credentials"))
	vaultToken := testFile(t, filepath.Join(home, ".vault-token"))
	httpieState := testDir(t, filepath.Join(home, ".config", "httpie"))
	paths := []string{
		npmCache, pnpmCLI, kubeConfig, gradleDaemon, rustupBin, goConfig, helmConfig, composerAuth,
		adbKey, denoCLI, bunCLI, podmanStorage, awsCredentials, vaultToken, httpieState,
	}

	catalog := testCatalog(t, nil)
	ordinary, err := Compile(catalog, testRequest(t, project))
	require.NoError(t, err)
	for _, path := range paths {
		_, ok := grantFor(ordinary, path)
		assert.False(t, ok, "dormant profile entry %q must have no default grant", path)
	}

	req := testRequest(t, project)
	req.GlobalEscalated = []string{
		"npm", "pnpm", "kubectl", "gradle", "cargo", "rustup", "go", "helm", "composer",
		"android-sdk-adb", "deno", "bun", "nix", "podman", "aws-cli", "vault", "httpie",
	}
	escalated, err := Compile(catalog, req)
	require.NoError(t, err)
	for _, path := range paths {
		grant, ok := grantFor(escalated, path)
		require.True(t, ok, "selected profile entry %q must be granted", path)
		assert.Equal(t, LevelEscalated, grant.Level)
	}
	keyGrant, ok := grantFor(escalated, adbKey)
	require.True(t, ok)
	assert.Equal(t, ModeReadOnly, keyGrant.Mode, "the ADB private key is an exact read-only grant")
	composerAuthGrant, ok := grantFor(escalated, composerAuth)
	require.True(t, ok)
	assert.Equal(t, ModeReadOnly, composerAuthGrant.Mode, "Composer auth is an exact read-only grant")
	kubeGrant, ok := grantFor(escalated, kubeConfig)
	require.True(t, ok)
	assert.Equal(t, ModeReadOnly, kubeGrant.Mode)
	for _, path := range []string{gradleDaemon, goConfig, helmConfig} {
		grant, found := grantFor(escalated, path)
		require.True(t, found)
		assert.Equal(t, ModeReadWrite, grant.Mode, "%q must support first-use state", path)
	}
	for _, path := range []string{pnpmCLI, denoCLI, bunCLI} {
		grant, found := grantFor(escalated, path)
		require.True(t, found)
		assert.Equal(t, ModeReadOnly, grant.Mode, "%q is an executable grant", path)
	}
	for _, path := range []string{awsCredentials, vaultToken} {
		grant, found := grantFor(escalated, path)
		require.True(t, found)
		assert.Equal(t, ModeReadOnly, grant.Mode, "%q is an exact credential grant", path)
	}
	httpieGrant, ok := grantFor(escalated, httpieState)
	require.True(t, ok)
	assert.Equal(t, ModeReadWrite, httpieGrant.Mode, "HTTPie persists session state by host")
	podmanStorageGrant, ok := grantFor(escalated, podmanStorage)
	require.True(t, ok)
	assert.Equal(t, ModeReadWrite, podmanStorageGrant.Mode)
	for _, socket := range []string{"/nix/var/nix/daemon-socket/socket", "/run/podman/podman.sock"} {
		grant, ok := socketFor(escalated, socket)
		require.True(t, ok, "selected high-authority socket %q must be granted", socket)
		assert.Equal(t, LevelEscalated, grant.Level)
	}

	for _, profile := range []string{"cargo", "rustup"} {
		request := testRequest(t, project)
		request.GlobalEscalated = []string{profile}
		policy, err := Compile(catalog, request)
		require.NoError(t, err)
		grant, ok := grantFor(policy, rustupBin)
		require.True(t, ok, "%s selection must grant the shared Cargo bin directory", profile)
		assert.Equal(t, profile, grant.Profile)
	}

	req.Shields = true
	shielded, err := Compile(catalog, req)
	require.NoError(t, err)
	for _, path := range paths {
		_, ok := grantFor(shielded, path)
		assert.False(t, ok, "raised shields remove selected profile entry %q", path)
	}
	assert.Empty(t, shielded.Sockets, "raised shields remove high-authority socket grants")
}

func TestShippedCatalog_OptionalProfilesCompose(t *testing.T) {
	home := testHome(t)
	project := testProject(t, home)
	catalog := testCatalog(t, nil)
	req := testRequest(t, project)
	active := map[string]bool{"shell": true, "mise": true, "git": true, "ssh": true, "gh": true}

	for _, name := range catalog.Names() {
		if !active[name] {
			req.GlobalEscalated = append(req.GlobalEscalated, name)
		}
	}

	_, err := Compile(catalog, req)
	require.NoError(t, err, "optional profiles must compose without conflicting grants")
}
