package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

// controllerapi mirrors the outbox type vocabulary as wire literals because it
// must not import sessionstore; this test is the pin that keeps them identical.
func TestOutputTypeVocabularyMatchesStore(t *testing.T) {
	assert.Equal(t, controllerapi.OutputMessageReplaceable, string(sessionstore.OutputMessageReplaceable))
	assert.Equal(t, controllerapi.OutputMessagePersistent, string(sessionstore.OutputMessagePersistent))
	assert.Equal(t, controllerapi.OutputSessionOpened, string(sessionstore.OutputSessionOpened))
	assert.Equal(t, controllerapi.OutputSessionReplaced, string(sessionstore.OutputSessionReplaced))
	assert.Equal(t, controllerapi.OutputSessionClosed, string(sessionstore.OutputSessionClosed))
}
