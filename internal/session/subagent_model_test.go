package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
)

// An unknown frontmatter `model:` otherwise fails at every spawn pointing at the
// model, not the definition file — so it has to be caught where the file is read.
func TestSubagentConfigs_UnknownModelIsReportedOnceAndDropped(t *testing.T) {
	ldr := loader.New()
	ldr.RegisterSubagent(&loader.Subagent{Name: "reviewer", Model: "sonnet", Path: "/p/.claude/agents/reviewer.md"})
	ldr.RegisterSubagent(&loader.Subagent{Name: "builder", Model: "known-model"})
	ldr.RegisterSubagent(&loader.Subagent{Name: "plain"})

	core, logs := observer.New(zapcore.WarnLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))

	configs := subagentConfigs(ctx, ldr, []config.ModelEntry{{ID: "known-model"}})

	byName := make(map[string]string, len(configs))
	for _, cfg := range configs {
		byName[string(cfg.Name)] = cfg.Model
	}

	require.Len(t, byName, 3, "an unknown model must not remove the subagent")
	assert.Empty(t, byName["reviewer"], "the unresolvable override is dropped, the agent stays")
	assert.Equal(t, "known-model", byName["builder"])
	assert.Empty(t, byName["plain"])

	entries := logs.FilterMessage("subagent_model_unknown").All()
	require.Len(t, entries, 1, "reported once at load, not once per spawn")
	assert.Contains(t, entries[0].ContextMap()["model"], "sonnet")
}

// With no catalog there is nothing to validate against; overrides pass through.
func TestSubagentConfigs_NoCatalogKeepsModelOverride(t *testing.T) {
	ldr := loader.New()
	ldr.RegisterSubagent(&loader.Subagent{Name: "reviewer", Model: "sonnet"})

	configs := subagentConfigs(t.Context(), ldr, nil)

	require.Len(t, configs, 1)
	assert.Equal(t, "sonnet", configs[0].Model)
}
