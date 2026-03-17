package llm

import (
	"encoding/json"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

// parseParamsSchema unmarshals a tool's Parameters into a map for SDK-specific
// assembly. Logs and returns ok=false on failure so callers can skip the tool.
func parseParamsSchema(name string, raw json.RawMessage) (map[string]any, bool) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		logger.Named("llm.toolschema").Warn("schema_parse_failed", zap.String("tool", name), zap.Error(err))
		return nil, false
	}

	return schema, true
}
