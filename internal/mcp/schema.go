package mcp

import (
	"encoding/json"
	"fmt"
)

const (
	schemaKeyType    = "type"
	schemaTypeObject = "object"
)

// Normalize bytes so dependency upgrades cannot expand the model-facing schema.
func normalizeToolSchema(raw []byte) ([]byte, error) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("decode tool schema: %w", err)
	}

	normalized := map[string]any{schemaKeyType: schemaTypeObject}

	for _, key := range []string{schemaKeyType, "properties", "required", "additionalProperties", "$defs"} {
		if value, ok := schema[key]; ok {
			normalized[key] = value
		}
	}

	if types, ok := normalized[schemaKeyType].([]any); ok {
		normalized[schemaKeyType] = schemaTypeObject

		for _, value := range types {
			if typ, ok := value.(string); ok && typ != "null" {
				normalized[schemaKeyType] = typ
				break
			}
		}
	}

	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("encode tool schema: %w", err)
	}

	return encoded, nil
}
