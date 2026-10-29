package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

func TestConfigStatus_WarnsOnceForRemovedManagerKeys(t *testing.T) {
	parsed, err := config.ParseUnifiedConfig([]byte(`managers:
  - service_topic_name: old
    assistant_topic_name: old
    service_topic_icon_emoji_id: old
    session_topic_icon_emoji_id: old
  - assistant_topic_name: repeated
`))
	require.NoError(t, err)
	var output bytes.Buffer

	logger.Init(logger.WithConsoleOutput(&output))
	defer logger.Init(logger.WithConsoleOutput(os.Stderr))
	logConfigStatus(&config.Config{UnifiedConfig: parsed})
	assert.Equal(t, 1, strings.Count(output.String(), "removed manager configuration keys ignored"))
	for _, key := range parsed.RemovedManagerKeys {
		assert.Equal(t, 1, strings.Count(output.String(), key))
	}
}
