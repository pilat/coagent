package builtin

import "strings"

// knownSearchServers orders MCP search tools in the prompt after websearch.
var knownSearchServers = []string{
	"tavily",
	"brave-search",
	"exa",
	"kagi",
	"searxng",
	"duckduckgo",
	"perplexity",
	"firecrawl",
}

// mcpSearchOrder matches on the whole tool ID, so a server name containing
// "search" makes every one of its tools a search tool.
func mcpSearchOrder(toolID string) (int, bool) {
	if !strings.Contains(toolID, "search") {
		return 0, false
	}

	for i, server := range knownSearchServers {
		if strings.HasPrefix(toolID, "mcp__"+server+"__") {
			return i + 1, true
		}
	}

	return 0, false
}
