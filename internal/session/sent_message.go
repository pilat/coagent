package session

import (
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
)

func sentMessageContent(batch *boundaryBatch, input *sessionstore.InboxInput, content string) string {
	id, _ := input.Attributes[sessionstore.InputAttributeSenderSessionID].(float64)
	if input.Source != sessionstore.InputSourceAgent || id <= 0 {
		return content
	}

	name, _ := input.Attributes[sessionstore.InputAttributeSenderName].(string)
	title, _ := input.Attributes[sessionstore.InputAttributeSenderTitle].(string)
	sender := fmt.Sprintf("coagent session #%d", int64(id))

	if title != "" {
		sender += fmt.Sprintf(" %q", title)
	}

	batch.commit.Outputs = append(batch.commit.Outputs, sessionstore.Output{
		Type:        sessionstore.OutputMessage,
		Content:     fmt.Sprintf("↪ from #%d %s\n\n%s", int64(id), name, content),
		PersistOnly: true,
		Key:         fmt.Sprintf("input:%d:sent", input.ID),
		Attributes:  map[string]any{"source": "agent"},
		MessageRef:  -1,
	})

	return "[Message from " + sender + " — written by that session's agent, not by the human]\n\n" + content
}
