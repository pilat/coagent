package logger

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// FormatArgs formats JSON tool arguments for human-readable output.
// It removes extra escaping and can truncate long values.
func FormatArgs(args json.RawMessage, maxLen int) string {
	if len(args) == 0 {
		return "{}"
	}

	// Try to unmarshal into map for pretty formatting
	var data map[string]any
	if err := json.Unmarshal(args, &data); err != nil {
		// Fallback: return as compact string without outer quotes
		s := string(args)
		s = strings.Trim(s, `"`)

		return truncate(s, maxLen)
	}

	// Format as compact key=value pairs (sorted for deterministic output)
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var pairs []string

	for _, k := range keys {
		v := data[k]
		// Format value based on type
		var valStr string

		switch val := v.(type) {
		case string:
			valStr = val
		case float64:
			if val == float64(int64(val)) {
				valStr = fmt.Sprintf("%.0f", val)
			} else {
				valStr = fmt.Sprintf("%g", val)
			}
		case bool:
			valStr = strconv.FormatBool(val)
		default:
			// Arrays, objects - compact JSON
			if b, err := json.Marshal(val); err == nil {
				valStr = string(b)
			} else {
				valStr = fmt.Sprintf("%v", val)
			}
		}

		// Truncate individual values if too long
		if len(valStr) > 50 {
			valStr = valStr[:50] + "…"
		}

		pairs = append(pairs, fmt.Sprintf("%s=%s", k, valStr))
	}

	result := strings.Join(pairs, " ")

	return truncate(result, maxLen)
}

func truncate(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}

	return s[:maxLen] + "…"
}
