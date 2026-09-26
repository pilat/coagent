package sandboxpolicy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

func TestValidateSection_AcceptsValidSection(t *testing.T) {
	home := testHome(t)
	cacheDir := filepath.Join(home, ".cache", "tool-a")
	socketPath := filepath.Join(home, "agent.sock")

	section := Section{
		Enabled:   true,
		Escalated: []string{"tool-a"},
		Projects:  map[string]ProjectOverride{filepath.Join(home, "project"): {Escalated: []string{"ssh"}}},
		Profiles: map[string]Profile{
			"tool-a": {
				Mounts:  []Mount{{Path: cacheDir, Mode: ModeReadWrite, Type: LevelEscalated}},
				Sockets: []Socket{{Path: socketPath, Type: LevelEscalated}},
				Network: []Network{
					{Address: HostLoopback, Protocol: ProtocolTCP, Ports: []int{5432}, Type: LevelEscalated},
				},
			},
		},
	}

	require.NoError(t, ValidateSection(section))
}

func TestValidateSection_RejectsUnknownProfile(t *testing.T) {
	testHome(t)

	section := Section{Enabled: true, Escalated: []string{"nope"}}
	assert.ErrorContains(t, ValidateSection(section), `unknown profile "nope"`)
}

func TestValidateSection_RejectsUnknownProjectProfile(t *testing.T) {
	home := testHome(t)

	section := Section{
		Enabled:  true,
		Projects: map[string]ProjectOverride{filepath.Join(home, "project"): {Escalated: []string{"nope"}}},
	}

	assert.ErrorContains(t, ValidateSection(section), `unknown profile "nope"`)
}

func TestValidateSection_RejectsInvalidEnums(t *testing.T) {
	testHome(t)

	tests := []struct {
		name    string
		profile Profile
		want    string
	}{
		{
			name:    "mount type",
			profile: Profile{Mounts: []Mount{{Path: "/srv/data", Mode: ModeReadOnly, Type: "sometimes"}}},
			want:    "invalid type",
		},
		{
			name:    "mount mode",
			profile: Profile{Mounts: []Mount{{Path: "/srv/data", Mode: "wo", Type: LevelBasic}}},
			want:    "invalid mode",
		},
		{
			name:    "mount kind",
			profile: Profile{Mounts: []Mount{{Path: "/srv/data", Mode: ModeReadOnly, Type: LevelBasic, Kind: "pipe"}}},
			want:    "invalid kind",
		},
		{
			name:    "socket type",
			profile: Profile{Sockets: []Socket{{Path: "/run/agent.sock", Type: "maybe"}}},
			want:    "invalid type",
		},
		{
			name: "network protocol",
			profile: Profile{
				Network: []Network{{Address: "10.0.0.1", Protocol: "sctp", Ports: []int{1}, Type: LevelBasic}},
			},
			want: "invalid protocol",
		},
		{
			name: "network port",
			profile: Profile{
				Network: []Network{{Address: "10.0.0.1", Protocol: ProtocolTCP, Ports: []int{0}, Type: LevelBasic}},
			},
			want: "invalid port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			section := Section{Enabled: true, Profiles: map[string]Profile{"custom": tt.profile}}
			assert.ErrorContains(t, ValidateSection(section), tt.want)
		})
	}
}

func TestValidateSection_RejectsBroadRoots(t *testing.T) {
	home := testHome(t)

	tests := []struct {
		name    string
		profile Profile
	}{
		{name: "filesystem root", profile: Profile{Mounts: []Mount{{Path: "/", Mode: ModeReadOnly, Type: LevelBasic}}}},
		{name: "home root", profile: Profile{Mounts: []Mount{{Path: home, Mode: ModeReadOnly, Type: LevelBasic}}}},
		{name: "whole tmp", profile: Profile{Mounts: []Mount{{Path: "/tmp", Mode: ModeReadWrite, Type: LevelBasic}}}},
		{name: "whole run", profile: Profile{Mounts: []Mount{{Path: "/run", Mode: ModeReadOnly, Type: LevelBasic}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			section := Section{Enabled: true, Profiles: map[string]Profile{"custom": tt.profile}}
			assert.Error(t, ValidateSection(section))
		})
	}
}

func TestValidateSection_RejectsDuplicateProjectKeys(t *testing.T) {
	home := testHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	alias := filepath.Join(home, "alias")
	require.NoError(t, symlink(project, alias))

	section := Section{
		Enabled:  true,
		Projects: map[string]ProjectOverride{project: {}, alias: {}},
	}

	assert.ErrorContains(t, ValidateSection(section), "select the same project")
}

func TestValidateSection_RejectsHomeExpandedDuplicateProjectKeys(t *testing.T) {
	home := testHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	section := Section{Enabled: true, Projects: map[string]ProjectOverride{
		project: {}, "~/project": {},
	}}
	assert.ErrorContains(t, ValidateSection(section), "select the same project")
}

func TestValidateSection_RejectsOptionalGrantThroughControlStateAlias(t *testing.T) {
	home := testHome(t)
	controlDir, err := coagenthome.Dir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(controlDir, 0o700))
	alias := filepath.Join(home, "alias")
	require.NoError(t, os.Symlink(controlDir, alias))
	section := Section{Enabled: true, Profiles: map[string]Profile{
		"bad": {Mounts: []Mount{{
			Path: filepath.Join(alias, "future-cache"), Mode: ModeReadOnly, Type: LevelBasic,
		}}},
	}}
	assert.ErrorContains(t, ValidateSection(section), "coagent control state")
}

func TestValidateSection_RejectsContradictoryDuplicates(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "cache")
	socketPath := filepath.Join(home, "agent.sock")

	tests := []struct {
		name    string
		profile Profile
	}{
		{
			name: "mount modes",
			profile: Profile{Mounts: []Mount{
				{Path: path, Mode: ModeReadOnly, Type: LevelBasic},
				{Path: path, Mode: ModeReadWrite, Type: LevelBasic},
			}},
		},
		{
			name: "mount downgrade",
			profile: Profile{Mounts: []Mount{
				{Path: path, Mode: ModeReadWrite, Type: LevelBasic},
				{Path: path, Mode: ModeReadOnly, Type: LevelEscalated},
			}},
		},
		{
			name: "socket levels",
			profile: Profile{Sockets: []Socket{
				{Path: socketPath, Type: LevelBasic},
				{Path: socketPath, Type: LevelEscalated},
			}},
		},
		{
			name: "network levels",
			profile: Profile{Network: []Network{
				{Address: HostLoopback, Protocol: ProtocolTCP, Ports: []int{5432}, Type: LevelBasic},
				{Address: HostLoopback, Protocol: ProtocolTCP, Ports: []int{5432}, Type: LevelEscalated},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			section := Section{Enabled: true, Profiles: map[string]Profile{"custom": tt.profile}}
			assert.Error(t, ValidateSection(section))
		})
	}
}

func TestValidateSection_AllowsRepeatedIdenticalEntries(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "cache")

	section := Section{
		Enabled: true,
		Profiles: map[string]Profile{"custom": {Mounts: []Mount{
			{Path: path, Mode: ModeReadOnly, Type: LevelBasic},
			{Path: path, Mode: ModeReadOnly, Type: LevelBasic},
		}}},
	}

	assert.NoError(t, ValidateSection(section))
}

func TestValidateSection_AllowsEscalatedMountPromotion(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, "installs")
	section := Section{Enabled: true, Profiles: map[string]Profile{
		"custom": {Mounts: []Mount{
			{Path: path, Mode: ModeReadOnly, Type: LevelBasic},
			{Path: path, Mode: ModeReadWrite, Type: LevelEscalated},
		}},
	}}
	require.NoError(t, ValidateSection(section))
}

func TestValidateSection_RejectsBroadLegacyWritablePath(t *testing.T) {
	testHome(t)

	assert.ErrorContains(
		t,
		ValidateSection(Section{Enabled: true, WritablePaths: []string{"/tmp"}}),
		"sandbox.writable_paths",
	)
}

func TestValidateSection_AcceptsNarrowLegacyWritablePath(t *testing.T) {
	home := testHome(t)

	assert.NoError(t, ValidateSection(Section{Enabled: true, WritablePaths: []string{filepath.Join(home, "work")}}))
}
