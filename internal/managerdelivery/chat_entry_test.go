package managerdelivery

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatLoop_RenameAppliesToNextOutputBatch(t *testing.T) {
	t.Parallel()
	fake := newChatFake()
	fake.focus = 12
	fake.sessions = []ChatSession{{ID: 12, Name: "old title"}}
	for id := int64(1); id <= chatBatchSize+1; id++ {
		fake.rows[12] = append(fake.rows[12], ChatRow{ID: id, Type: "message", Content: fmt.Sprint(id)})
	}
	fake.onSend = func(string) { fake.sessions[0].Name = "new title" }
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	require.Len(t, fake.sent, chatBatchSize+1)
	for i, text := range fake.sent[:chatBatchSize] {
		assert.Equal(t, fmt.Sprintf("#12 old title\n%d", i+1), text)
	}
	assert.Equal(t, "#12 new title\n101", fake.sent[chatBatchSize])
}
