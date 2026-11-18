package session

import (
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
)

func signalNotice(batch *boundaryBatch, input *sessionstore.InboxInput) {
	notice := inputSignalNotice(input)
	if notice == "" {
		return
	}

	batch.commit.Outputs = append(batch.commit.Outputs, sessionstore.Output{
		Type:        sessionstore.OutputMessage,
		Content:     notice,
		PersistOnly: true,
		Key:         fmt.Sprintf("input:%d:signal", input.ID),
		MessageRef:  -1,
	})
}

func inputSignalNotice(input *sessionstore.InboxInput) string {
	if input.Source != sessionstore.InputSourceSubagent {
		return ""
	}

	notice, _ := input.Attributes[sessionstore.InputAttributeSignalNotice].(string)

	return notice
}
