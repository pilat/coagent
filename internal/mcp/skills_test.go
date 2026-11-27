package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
)

func TestClientSkills_ListsDeclaredServerAcrossPages(t *testing.T) {
	c, dir := skillsClient(t, true, false)
	writeSkillFixture(t, dir, "skills.json", map[string]any{"skills": []any{skillEntry("pdf")}, "nextCursor": "second"})
	writeSkillFixture(t, dir, "skills-second.json", map[string]any{"skills": []any{skillEntry("guide")}})
	c.refreshSkills(t.Context())
	require.Len(t, c.Skills(), 2)
	require.Equal(t, "pdf", c.Skills()[0].Name)
	require.Equal(t, "guide", c.Skills()[1].Name)
	require.Equal(t, "skill://pdf/SKILL.md", c.Skills()[0].Files[0].URI)
	require.Equal(t, "Use pdf", c.Skills()[0].Frontmatter["description"])
	require.NoError(t, c.Skills()[0].Files[0].Verify([]byte("abc")))
	require.NoError(t, c.Skills()[1].Files[0].Verify([]byte("abc")))
}

func TestClientSkills_UndeclaredServerIsNeverAsked(t *testing.T) {
	c, dir := skillsClient(t, false, false)
	c.refreshSkills(t.Context())
	require.Empty(t, c.Skills())
	raw, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "skills/list")
}

func TestClientSkills_ModernRequestCarriesProtocolMeta(t *testing.T) {
	c, _ := skillsClient(t, true, true)
	c.refreshSkills(t.Context())
	require.Len(t, c.Skills(), 1)
}

func TestClientSkills_DeclinesInvalidEntries(t *testing.T) {
	for _, tc := range invalidSkillEntries() {
		t.Run(tc.reason, func(t *testing.T) {
			c, dir := skillsClient(t, true, false)
			entry := skillEntry("guide")
			tc.change(entry)
			writeSkillFixture(t, dir, "skills.json", map[string]any{"skills": []any{skillEntry("pdf"), entry}})
			core, logs := observer.New(zap.WarnLevel)
			c.refreshSkills(logger.ToContext(t.Context(), zap.New(core)))
			require.Len(t, c.Skills(), 1)
			require.Equal(t, "pdf", c.Skills()[0].Name)
			warnings := logs.FilterMessage("mcp_skill_declined").All()
			require.Len(t, warnings, 1)
			fields := warnings[0].ContextMap()
			if tc.decodeFailure {
				reason, ok := fields["reason"].(string)
				require.True(t, ok)
				require.True(t, strings.HasPrefix(reason, tc.reason), "reason %q must start with %q", reason, tc.reason)
				require.Empty(t, fields["uri"])
			} else {
				require.Equal(t, tc.reason, fields["reason"])
				require.Equal(t, entry["uri"], fields["uri"])
			}
		})
	}
}

func TestClientSkills_ListFailureKeepsTools(t *testing.T) {
	for _, failure := range []string{"rpc", "decode", "incomplete", "cursor"} {
		t.Run(failure, func(t *testing.T) {
			dir, cfg := skillsFixture(t, true, false)
			switch failure {
			case "rpc":
				require.NoError(t, os.WriteFile(filepath.Join(dir, "skills-error"), nil, 0o600))
			case "decode":
				require.NoError(t, os.WriteFile(filepath.Join(dir, "skills.json"), []byte(`{"skills":true}`), 0o600))
			case "incomplete":
				writeSkillFixture(t, dir, "skills.json", map[string]any{"skills": []any{}, "resultType": "pending"})
			case "cursor":
				page := map[string]any{"skills": []any{skillEntry("pdf")}, "nextCursor": "again"}
				writeSkillFixture(t, dir, "skills.json", page)
				writeSkillFixture(t, dir, "skills-again.json", page)
			}
			core, logs := observer.New(zap.WarnLevel)
			service := New("", nil, nil)
			t.Cleanup(service.Stop)
			stats, err := service.Start(
				logger.ToContext(t.Context(), zap.New(core)),
				&Config{Servers: map[string]ServerConfig{"docs": cfg}},
			)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Started)
			require.Contains(t, service.GetClient("docs").Tools(), "view")
			require.Empty(t, service.Skills())
			require.Equal(t, 1, logs.FilterMessage("mcp_skills_list_failed").Len())
		})
	}
}

func TestServiceRefresh_RelistsSkillsOnTheLiveProcess(t *testing.T) {
	dir, cfg := skillsFixture(t, true, false)
	service := New("", nil, nil)
	t.Cleanup(service.Stop)
	_, err := service.Start(t.Context(), &Config{Servers: map[string]ServerConfig{"docs": cfg}})
	require.NoError(t, err)
	require.Equal(t, "pdf", service.Skills()[0].Skills[0].Name)
	writeSkillFixture(t, dir, "skills.json", map[string]any{"skills": []any{skillEntry("guide")}})
	require.True(t, service.Refresh(t.Context()))
	require.Equal(t, "guide", service.Skills()[0].Skills[0].Name)
	raw, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(raw), "initialize\n"))
	require.Equal(t, 2, strings.Count(string(raw), "skills/list\n"))
}
