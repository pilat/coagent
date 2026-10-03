package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
)

const untrustedProbeMarker = "PROBE_UNTRUSTED"

// requestRecorder keeps each request's full message transcript, in order.
type requestRecorder struct {
	mu   sync.Mutex
	msgs [][]llmwire.Message
}

func (r *requestRecorder) record(msgs []llmwire.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.msgs = append(r.msgs, msgs)
}

func (r *requestRecorder) lastMessages(t *testing.T) []llmwire.Message {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	require.NotEmpty(t, r.msgs, "no scripted model request was recorded")

	return r.msgs[len(r.msgs)-1]
}

// The wrapper and the guidance are checked against the real daemon/session
// stack: registry → session formatting → durable transcript → next model
// input → next request messages. webfetch on a local httptest server keeps the
// scenario hermetic; the scripted model fetches the local page once, then
// finishes.
func TestHarnessScenario_UntrustedToolOutputCarriesWrapperAndGuidance(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(untrustedProbeMarker + " page body"))
	}))
	t.Cleanup(page.Close)

	prompts := newPromptRecorder()
	requests := &requestRecorder{}

	respond := func(system string, msgs []llmwire.Message) *llmwire.Response {
		prompts.record("root", system)
		requests.record(msgs)

		if hasToolResultFor(msgs, "webfetch") {
			return &llmwire.Response{Text: "untrusted probe done"}
		}

		args, _ := json.Marshal(map[string]string{"url": page.URL})

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "fetch-1", Name: "webfetch", Arguments: args,
		}}}
	}

	h := newGatingHarness(t, nil, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "probe untrusted output", "fake-model", nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return strings.Contains(lastToolResultContent(h.parentMessages(parentID), "webfetch"),
			untrustedProbeMarker)
	}, 10*time.Second, 20*time.Millisecond, "the webfetch result must reach the transcript")
	h.mgr.waitIdle(parentID)

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "transcript must stay provider-valid")

	content := lastToolResultContent(msgs, "webfetch")
	markers := regexp.MustCompile(`<<<BEGIN_UNTRUSTED_EXTERNAL_DATA id="([0-9a-f]{16})">>>`).FindStringSubmatch(content)
	require.Len(t, markers, 2)
	assert.True(t, strings.HasSuffix(content, `<<<END_UNTRUSTED_EXTERNAL_DATA id="`+markers[1]+`">>>`))
	assert.Contains(t, content, untrustedProbeMarker)

	// The next model request re-derives its messages from the durable
	// transcript: the persisted wrapper must survive into that request too.
	requestMsgs := requests.lastMessages(t)
	requestContent := lastToolResultContent(requestMsgs, "webfetch")
	assert.Equal(t, content, requestContent)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "continue the probe"))
	require.Eventually(t, func() bool {
		return hasUserContaining(requests.lastMessages(t), "continue the probe")
	}, 10*time.Second, 20*time.Millisecond, "the next activation must reach the model")
	h.mgr.waitIdle(parentID)
	assert.Equal(t, content, lastToolResultContent(requests.lastMessages(t), "webfetch"))
	assert.Equal(t, content, lastToolResultContent(h.parentMessages(parentID), "webfetch"))

	// The system prompt carries the matching dynamic guidance for the
	// registered toolset.
	prompt := prompts.last(t, "root")
	assert.Contains(t, prompt, "# UNTRUSTED CONTENT")
	assert.Contains(t, prompt, "curl")
	assert.Contains(t, prompt, "wget")
	assert.Contains(t, prompt, `<<<BEGIN_UNTRUSTED_EXTERNAL_DATA id="...">>>`)
	assert.Contains(t, prompt, "same ID")
}
