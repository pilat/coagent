package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixture isolates HOME under a fresh temp dir and gives back a project root,
// a work directory and a ready-to-use ~/.ssh layout for rule tests.
type fixture struct {
	home    string
	project string
	workDir string
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	project := t.TempDir()
	workDir := t.TempDir()

	return fixture{home: home, project: project, workDir: workDir}
}

func (f fixture) sshDir(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(f.home, ".ssh")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	return dir
}

func (f fixture) writeFile(t *testing.T, path, contents string) string {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	return path
}

func TestCompile_HostIsReadableAndProjectIsWritableByDefault(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()

	policy, err := Compile(Request{ProjectRoot: f.project, WorkDir: f.project})
	require.NoError(t, err)

	assert.True(t, policy.AllowsRead(outside))
	assert.False(t, policy.AllowsWrite(outside))
	assert.True(t, policy.AllowsRead(f.project))
	assert.True(t, policy.AllowsWrite(f.project))
}

func TestCompile_ProcessOutputOverridesOperatorRules(t *testing.T) {
	f := newFixture(t)
	output := filepath.Join(f.home, ".coagent", "processes", "project-1")
	require.NoError(t, os.MkdirAll(output, 0o700))
	record := filepath.Join(output, "1", "process.output")

	for name, rule := range map[string]Rule{
		"deny":  {Deny: record},
		"write": {Allow: output, Mode: ModeReadWrite},
	} {
		t.Run(name, func(t *testing.T) {
			policy, err := Compile(Request{
				ProjectRoot: f.project, WorkDir: f.project, ProcessOutputRoot: output,
				GlobalRules:  []Rule{{Deny: filepath.Dir(filepath.Dir(output))}},
				ProjectRules: []Rule{rule},
			})
			require.NoError(t, err)
			assert.True(t, policy.AllowsRead(record))
			assert.False(t, policy.AllowsWrite(record))
			assert.True(t, policy.AllowsRead(filepath.Join(output, "2", "child.output")))
			assert.False(t, policy.AllowsRead(filepath.Join(f.home, ".coagent", "secrets")))
			assert.False(t, policy.AllowsRead(filepath.Join(filepath.Dir(output), "project-2", "other.output")))
		})
	}

	withoutOutput, err := Compile(Request{ProjectRoot: f.project, WorkDir: f.project})
	require.NoError(t, err)
	withOutput, err := Compile(Request{ProjectRoot: f.project, WorkDir: f.project, ProcessOutputRoot: output})
	require.NoError(t, err)
	assert.NotEqual(t, withoutOutput.Digest, withOutput.Digest)
}

func TestCompile_DenyDirectoryThenAllowFileCarvesOutOneFileOnly(t *testing.T) {
	f := newFixture(t)
	ssh := f.sshDir(t)
	knownHosts := f.writeFile(t, filepath.Join(ssh, "known_hosts"), "host-key")
	idRSA := f.writeFile(t, filepath.Join(ssh, "id_rsa"), "private-key")

	policy, err := Compile(Request{
		ProjectRoot: f.project,
		WorkDir:     f.project,
		GlobalRules: []Rule{
			{Deny: "~/.ssh"},
			{Allow: "~/.ssh/known_hosts"},
		},
	})
	require.NoError(t, err)

	assert.False(t, policy.AllowsRead(idRSA), "an unrelated file under the denied dir stays denied")
	assert.True(t, policy.AllowsRead(knownHosts))
}

func TestCompile_ReversedRuleOrderFlipsTheOutcome(t *testing.T) {
	f := newFixture(t)
	ssh := f.sshDir(t)
	knownHosts := f.writeFile(t, filepath.Join(ssh, "known_hosts"), "host-key")

	policy, err := Compile(Request{
		ProjectRoot: f.project,
		WorkDir:     f.project,
		GlobalRules: []Rule{
			{Allow: "~/.ssh/known_hosts"},
			{Deny: "~/.ssh"},
		},
	})
	require.NoError(t, err)

	assert.False(t, policy.AllowsRead(knownHosts), "the later, broader deny wins over the earlier narrow allow")
}

func TestCompile_LaterBroadRuleOverridesEarlierNarrowRule(t *testing.T) {
	f := newFixture(t)
	cacheDir := filepath.Join(f.home, ".cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0o700))
	nested := filepath.Join(cacheDir, "tool")
	require.NoError(t, os.MkdirAll(nested, 0o700))

	policy, err := Compile(Request{
		ProjectRoot: f.project,
		WorkDir:     f.project,
		GlobalRules: []Rule{
			{Deny: "~/.cache/tool"},
			{Allow: "~/.cache", Mode: ModeReadWrite},
		},
	})
	require.NoError(t, err)

	assert.True(t, policy.AllowsWrite(nested), "the later, broader allow overrides the earlier narrow deny")
}

func TestCompile_ProjectRulesAreEvaluatedAfterGlobalRulesAndCanOverrideThem(t *testing.T) {
	f := newFixture(t)
	awsDir := filepath.Join(f.home, ".aws")
	require.NoError(t, os.MkdirAll(awsDir, 0o700))
	creds := f.writeFile(t, filepath.Join(awsDir, "credentials"), "secret")

	policy, err := Compile(Request{
		ProjectRoot:  f.project,
		WorkDir:      f.project,
		GlobalRules:  []Rule{{Deny: "~/.aws"}},
		ProjectRules: []Rule{{Allow: "~/.aws/credentials"}},
	})
	require.NoError(t, err)

	assert.True(t, policy.AllowsRead(creds), "the project's own rule outranks the global deny")
}

func TestCompile_ReadWriteAllowGrantsWritesButReadOnlyAllowDoesNot(t *testing.T) {
	f := newFixture(t)
	modCache := filepath.Join(f.home, "go", "pkg", "mod")
	require.NoError(t, os.MkdirAll(modCache, 0o700))
	knownHosts := f.writeFile(t, f.sshDir(t)+"/known_hosts", "host-key")

	policy, err := Compile(Request{
		ProjectRoot: f.project,
		WorkDir:     f.project,
		GlobalRules: []Rule{
			{Allow: "~/go/pkg/mod", Mode: ModeReadWrite},
			{Allow: "~/.ssh/known_hosts"},
		},
	})
	require.NoError(t, err)

	assert.True(t, policy.AllowsWrite(modCache))
	assert.True(t, policy.AllowsRead(knownHosts))
	assert.False(t, policy.AllowsWrite(knownHosts))
}

func TestCompile_AbsentDenyFailsClosed(t *testing.T) {
	f := newFixture(t)
	missing := filepath.Join(f.home, "does-not-exist")

	_, err := Compile(Request{
		ProjectRoot: f.project,
		WorkDir:     f.project,
		GlobalRules: []Rule{{Deny: missing}},
	})
	require.ErrorContains(t, err, "does not exist")
}

func TestCompile_DigestChangesWithRuleOrderAndIsStableForIdenticalInput(t *testing.T) {
	f := newFixture(t)
	ssh := f.sshDir(t)
	f.writeFile(t, filepath.Join(ssh, "known_hosts"), "host-key")

	req := Request{
		ProjectRoot: f.project,
		WorkDir:     f.project,
		GlobalRules: []Rule{
			{Deny: "~/.ssh"},
			{Allow: "~/.ssh/known_hosts"},
		},
	}

	first, err := Compile(req)
	require.NoError(t, err)
	second, err := Compile(req)
	require.NoError(t, err)
	assert.Equal(t, first.Digest, second.Digest)

	reversed := req
	reversed.GlobalRules = []Rule{req.GlobalRules[1], req.GlobalRules[0]}
	third, err := Compile(reversed)
	require.NoError(t, err)
	assert.NotEqual(t, first.Digest, third.Digest)
}

func TestCompile_ExplicitDenyNamingTheProjectRootExactlyIsHonoured(t *testing.T) {
	f := newFixture(t)

	policy, err := Compile(Request{
		ProjectRoot:  f.project,
		WorkDir:      f.project,
		ProjectRules: []Rule{{Deny: f.project}},
	})
	require.NoError(t, err)
	assert.False(t, policy.AllowsWrite(f.project))
}

func TestCompile_BroadDenyOfAnAncestorStillFails(t *testing.T) {
	f := newFixture(t)
	ancestor := filepath.Dir(f.project)

	_, err := Compile(Request{
		ProjectRoot:  f.project,
		WorkDir:      f.project,
		ProjectRules: []Rule{{Deny: ancestor}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("sandbox rules leave the project root %q unwritable", f.project))
	assert.Contains(t, err.Error(), fmt.Sprintf("rule 0 (deny %q) applies after it", ancestor))
}

func TestCompile_GlobalBroadDenyDoesNotAffectProjectWritability(t *testing.T) {
	f := newFixture(t)
	sibling := filepath.Join(f.home, "sibling")
	require.NoError(t, os.MkdirAll(sibling, 0o700))

	policy, err := Compile(Request{
		ProjectRoot: f.project,
		WorkDir:     f.project,
		GlobalRules: []Rule{{Deny: "~/"}},
	})
	require.NoError(t, err)
	assert.True(t, policy.AllowsWrite(f.project), "the project default is emitted after the global section and wins")
	assert.False(
		t,
		policy.AllowsRead(sibling),
		"a path under the same ancestor is denied, proving the rule took effect",
	)
}

func TestCompile_UnrelatedDenyDoesNotAffectProjectWritability(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()

	_, err := Compile(Request{
		ProjectRoot: f.project,
		WorkDir:     f.project,
		GlobalRules: []Rule{{Deny: outside}},
	})
	require.NoError(t, err)
}

func TestCompile_AssemblesEntriesInTheDocumentedOrder(t *testing.T) {
	f := newFixture(t)
	workDir := f.workDir
	gitDir := t.TempDir()
	globalRuleDir := t.TempDir()
	projectRuleDir := t.TempDir()

	policy, err := Compile(Request{
		ProjectRoot:    f.project,
		WorkDir:        workDir,
		WorktreeGitDir: gitDir,
		GlobalRules:    []Rule{{Deny: globalRuleDir}},
		ProjectRules:   []Rule{{Deny: projectRuleDir}},
	})
	require.NoError(t, err)

	paths := make([]string, 0, len(policy.Entries))
	for _, entry := range policy.Entries {
		paths = append(paths, entry.Path)
	}

	assert.Equal(t, []string{
		string(filepath.Separator),
		globalRuleDir,
		f.project,
		workDir,
		gitDir,
		projectRuleDir,
	}, paths)
}

func TestCompile_WorktreeGitDirIsWritableWhenSetAndAbsentWhenEmpty(t *testing.T) {
	f := newFixture(t)
	gitDir := t.TempDir()

	withGitDir, err := Compile(Request{ProjectRoot: f.project, WorkDir: f.project, WorktreeGitDir: gitDir})
	require.NoError(t, err)
	assert.True(t, withGitDir.AllowsWrite(gitDir))

	withoutGitDir, err := Compile(Request{ProjectRoot: f.project, WorkDir: f.project})
	require.NoError(t, err)

	for _, entry := range withoutGitDir.Entries {
		assert.NotEqual(t, gitDir, entry.Path)
	}
}

func TestCompile_OperatorRuleCanStillDenyTheWorktreeGitDir(t *testing.T) {
	f := newFixture(t)
	gitDir := t.TempDir()

	policy, err := Compile(Request{
		ProjectRoot:    f.project,
		WorkDir:        f.project,
		WorktreeGitDir: gitDir,
		ProjectRules:   []Rule{{Deny: gitDir}},
	})
	require.NoError(t, err)
	assert.False(t, policy.AllowsRead(gitDir))
	assert.False(t, policy.AllowsWrite(gitDir))
}

func TestSelectProjectRules_OnlyTheExactlyMatchingProjectKeyApplies(t *testing.T) {
	f := newFixture(t)
	other := t.TempDir()

	section := Section{
		Enabled: true,
		Projects: map[string]ProjectRules{
			f.project: {Rules: []Rule{{Allow: "/var/run/docker.sock", Mode: ModeReadWrite}}},
			other:     {Rules: []Rule{{Deny: "/etc/shadow"}}},
		},
	}

	rules, err := SelectProjectRules(section, f.project, "")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "/var/run/docker.sock", rules[0].Allow)
}

func TestSelectProjectRules_WorktreeInheritsSourceRepositoryRulesBeforeItsOwn(t *testing.T) {
	f := newFixture(t)
	source := t.TempDir()

	section := Section{
		Projects: map[string]ProjectRules{
			source:    {Rules: []Rule{{Deny: "/etc/shadow"}}},
			f.project: {Rules: []Rule{{Allow: "/var/run/docker.sock", Mode: ModeReadWrite}}},
		},
	}

	rules, err := SelectProjectRules(section, f.project, source)
	require.NoError(t, err)
	require.Len(t, rules, 2)
	assert.Equal(t, "/etc/shadow", rules[0].Deny)
	assert.Equal(t, "/var/run/docker.sock", rules[1].Allow)
}

func TestValidateSection_BothAllowAndDenySetIsRejected(t *testing.T) {
	err := ValidateSection(Section{Rules: []Rule{{Allow: "/a", Deny: "/b"}}})
	assert.Error(t, err)
}

func TestValidateSection_NeitherAllowNorDenySetIsRejected(t *testing.T) {
	err := ValidateSection(Section{Rules: []Rule{{}}})
	assert.Error(t, err)
}

func TestValidateSection_ModeOnDenyRuleIsRejected(t *testing.T) {
	err := ValidateSection(Section{Rules: []Rule{{Deny: "/a", Mode: ModeReadWrite}}})
	assert.Error(t, err)
}

func TestValidateSection_BadModeValueIsRejected(t *testing.T) {
	err := ValidateSection(Section{Rules: []Rule{{Allow: "/a", Mode: "rx"}}})
	assert.Error(t, err)
}

func TestValidateSection_RelativePathIsRejected(t *testing.T) {
	err := ValidateSection(Section{Rules: []Rule{{Allow: "relative/path"}}})
	assert.Error(t, err)
}

func TestValidateSection_DotDotTraversalIsRejected(t *testing.T) {
	err := ValidateSection(Section{Rules: []Rule{{Allow: "/a/../b"}}})
	assert.Error(t, err)
}

func TestValidateSection_TwoProjectKeysNamingTheSameProjectAreRejected(t *testing.T) {
	err := ValidateSection(Section{
		Projects: map[string]ProjectRules{
			"/home/example/project":  {},
			"/home/example/project/": {},
		},
	})
	assert.Error(t, err)
}

func TestValidateSection_ValidSectionPasses(t *testing.T) {
	err := ValidateSection(Section{
		Enabled: true,
		Rules: []Rule{
			{Deny: "~/.ssh"},
			{Allow: "~/.ssh/known_hosts"},
			{Allow: "~/.cache", Mode: ModeReadWrite},
		},
		Projects: map[string]ProjectRules{
			"/home/example/projects/service": {
				Rules: []Rule{{Allow: "/var/run/docker.sock", Mode: ModeReadWrite}},
			},
		},
	})
	assert.NoError(t, err)
}

func TestCanonicalProjectAliasesAreRejected(t *testing.T) {
	f := newFixture(t)
	f.project = filepath.Join(f.home, "project")
	require.NoError(t, os.Mkdir(f.project, 0o700))
	link := filepath.Join(f.home, "alias")
	require.NoError(t, os.Symlink(f.project, link))
	rel, err := filepath.Rel(f.home, f.project)
	require.NoError(t, err)
	for _, alias := range []string{"~/" + rel, link} {
		section := Section{Projects: map[string]ProjectRules{
			f.project: {Rules: []Rule{{Allow: f.project}}},
			alias:     {Rules: []Rule{{Deny: f.project}}},
		}}
		require.ErrorContains(t, ValidateSection(section), "same project")
		_, err := SelectProjectRules(section, f.project, "")
		require.ErrorContains(t, err, "same project")
	}
}

func TestCompileCanonicalizesRuleAliases(t *testing.T) {
	f := newFixture(t)
	secret := f.writeFile(t, filepath.Join(f.home, "secret"), "private")
	link := filepath.Join(f.home, "alias")
	require.NoError(t, os.Symlink(f.home, link))
	policy, err := Compile(Request{
		ProjectRoot: f.project, WorkDir: f.project,
		GlobalRules: []Rule{{Deny: filepath.Join(link, "secret")}},
	})
	require.NoError(t, err)
	assert.False(t, policy.AllowsRead(secret))
	assert.False(t, policy.AllowsRead("relative"))
	assert.Equal(t, secret, policy.Entries[1].Path)
}

func TestCompileIgnoresSupersededAbsentDeny(t *testing.T) {
	f := newFixture(t)
	missing := filepath.Join(f.project, "missing")
	policy, err := Compile(Request{
		ProjectRoot: f.project, WorkDir: f.project,
		ProjectRules: []Rule{{Deny: missing}, {Allow: f.project, Mode: ModeReadWrite}},
	})
	require.NoError(t, err)
	assert.True(t, policy.AllowsWrite(missing))
}

func TestCompileRefusesAbsentReadOnlyCarveOut(t *testing.T) {
	f := newFixture(t)
	_, err := Compile(Request{
		ProjectRoot: f.project, WorkDir: f.project,
		ProjectRules: []Rule{{Allow: filepath.Join(f.project, "missing")}},
	})
	require.ErrorContains(t, err, "does not exist beneath a writable grant")
}
