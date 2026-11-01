package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
)

func TestChatRowPreservesRelayOrigin(t *testing.T) {
	assert.Equal(t, managerdelivery.ChatRow{
		ID: 9, Name: "Assistant", Type: "message", Content: "answer",
		Origin: 42, OriginName: "project · purpose",
	}, chatRow(controllerapi.ChatOutput{
		ID: 9, Name: "Assistant", Type: "message", Content: "answer", Origin: 42, OriginName: "project · purpose",
	}))
}
