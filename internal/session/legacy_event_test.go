package session

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/pilat/coagent/internal/id"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

const subagentEventTool = "subagent_event"

// BuildBackgroundSubagentCompletion remains test-only while historical
// transcript fixtures cover compaction and repair of already persisted events.
func BuildBackgroundSubagentCompletion(childID int64, content string) ([]*transcript.Message, error) {
	if childID <= 0 {
		return nil, fmt.Errorf("positive child id is required")
	}
	callID := id.Generate()
	calls, err := json.Marshal([]llmwire.ToolCall{{
		ID: callID, Name: subagentEventTool, Arguments: []byte(fmt.Sprintf(`{"child_id":%d}`, childID)),
	}})
	if err != nil {
		return nil, err
	}
	return []*transcript.Message{{Role: llmwire.RoleAssistant, ToolCalls: calls}, {
		Role: llmwire.RoleTool, Content: content, ToolCallID: callID, ToolName: subagentEventTool,
	}}, nil
}

func TestLegacyEventFixture(t *testing.T) {}
