package sessionbuild

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/registry"
)

func TestToolsSectionBytes_MCPGuidance(t *testing.T) {
	for _, binding := range []*config.BrowserConfig{nil, {MCPName: "remote"}} {
		mode := "ordinary"
		if binding != nil {
			mode = "bound"
		}
		t.Run(mode, func(t *testing.T) {
			servers := mcpServerFixture(t, map[string][]string{
				"remote": {"view", "act"}, "alpha": {"lookup"}, "silent": {"view"}, "empty": {},
			}, map[string]string{
				"remote": "Use the latest frame.\n# Server heading\nCoordinates change after each action.",
				"alpha":  "Use alpha for lookups.", "empty": "No visible tools, no guidance.",
			})
			reg, frames := realRegistry(t, false, servers, binding)
			set := registry.NewSet(nil)
			for _, agent := range []registry.AgentType{registry.AgentTypeBuild, registry.AgentTypeGeneral, registry.AgentTypeBrowser} {
				cfg, ok := set.Get(agent)
				require.True(t, ok)
				filtered := filterRegistryForAgent(set, reg, cfg, frames.Owns)
				text := renderToolsSection(filtered, false)
				assertToolsSectionGolden(t, "guidance-"+mode+"-"+string(agent), text)
				wantRemote := (binding != nil) == (agent == registry.AgentTypeBrowser)
				assert.Equal(t, wantRemote, strings.Contains(text, "Use the latest frame."))
				assert.Equal(t, agent != registry.AgentTypeBrowser, strings.Contains(text, "Use alpha for lookups."))
				assert.NotContains(t, text, "No visible tools, no guidance.")
				assert.NotContains(t, text, `begin guidance from MCP server "silent"`)
				if wantRemote {
					assert.Equal(t, 1, strings.Count(text, `begin guidance from MCP server "remote"`))
					assert.Contains(
						t,
						text,
						"# Server heading\nCoordinates change after each action.\n--- end guidance",
					)
					if agent != registry.AgentTypeBrowser {
						assert.Less(t, strings.Index(text, `begin guidance from MCP server "alpha"`),
							strings.Index(text, `begin guidance from MCP server "remote"`))
					}
				}
				assert.Equal(t, text, renderToolsSection(filtered, false))
			}
		})
	}
}
