package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// skillRecorder captures what every scripted provider call was handed — the
// system prompt and the transcript — so a test can assert on the request the
// model actually received rather than on the stored transcript alone.
type skillRecorder struct {
	mu    sync.Mutex
	calls []skillCall
}

type skillCall struct {
	system string
	msgs   []llmwire.Message
}

func (r *skillRecorder) wrap(
	respond func(string, []llmwire.Message) *llmwire.Response,
) func(string, []llmwire.Message) *llmwire.Response {
	return func(system string, msgs []llmwire.Message) *llmwire.Response {
		r.mu.Lock()
		r.calls = append(r.calls, skillCall{system: system, msgs: append([]llmwire.Message(nil), msgs...)})
		r.mu.Unlock()

		return respond(system, msgs)
	}
}

func (r *skillRecorder) snapshot() []skillCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]skillCall(nil), r.calls...)
}

// newSkillHarness is the subagent harness over a project WorkDir carrying
// .claude/skills. Skills load when a session is created, so writing them before
// the first Send is enough.
func newSkillHarness(
	t *testing.T,
	skills map[string]string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) (*subagentHarness, *skillRecorder) {
	t.Helper()

	rec := &skillRecorder{}
	h := newSubagentHarnessWith(t, rec.wrap(respond))

	for name, body := range skills {
		dir := filepath.Join(h.workDir(), ".claude", "skills", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600))
	}

	return h, rec
}

func (h *subagentHarness) workDir() string {
	h.t.Helper()

	workDir, err := h.mgr.GetProjectWorkDir(h.ctx, h.projectID)
	require.NoError(h.t, err)

	return workDir
}

// plainRespond answers every turn with text, so a session reaches idle in one turn.
func plainRespond(_ string, _ []llmwire.Message) *llmwire.Response {
	return &llmwire.Response{Text: "work done"}
}

func skillDoc(name, description, body string, extraFrontmatter ...string) string {
	var b strings.Builder

	b.WriteString("---\nname: " + name + "\ndescription: " + description + "\n")

	for _, line := range extraFrontmatter {
		b.WriteString(line + "\n")
	}

	b.WriteString("---\n" + body + "\n")

	return b.String()
}

// countMessagesWithSkill counts transcript rows carrying the named skill envelope.
func countMessagesWithSkill(msgs []llmwire.Message, name string) int {
	count := 0

	for _, m := range msgs {
		count += strings.Count(m.Content, "<name>"+name+"</name>")
	}

	return count
}

// warningNotices returns every ⚠️ line a controller saw for the session.
func warningNotices(events []controllerapi.SessionNotification, sessionID int64) []string {
	var out []string

	for _, event := range events {
		if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}

		if strings.HasPrefix(event.Notification.Message, "⚠️ ") {
			out = append(out, event.Notification.Message)
		}
	}

	return out
}

func (h *subagentHarness) requireInboxDrained(sessionID int64) {
	h.t.Helper()

	_, err := h.sessStore.PeekPending(h.ctx, sessionID)
	require.ErrorIs(h.t, err, sessionstore.ErrNoPendingInput, "the rejected input must leave the inbox")
}

// A /skill for a skill that does not exist is resolved on the control plane: the
// durable row is consumed, the human is told once, and the model is never asked
// to answer a command that never became a message.
func TestHarnessScenario_UnknownSkillCommandIsRejectedOnceAndDrains(t *testing.T) {
	h, rec := newSkillHarness(t, nil, plainRespond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "warm up", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)

	callsBefore := len(rec.snapshot())

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/skill nonexistent"))
	collector.waitFor(t, "rejection notice reaches the controller", func(e []controllerapi.SessionNotification) bool {
		return len(warningNotices(e, sessionID)) > 0
	})
	h.mgr.waitIdle(sessionID)

	notices := warningNotices(collector.snapshot(), sessionID)
	require.Len(t, notices, 1, "exactly one rejection notice")
	assert.Contains(t, notices[0], "skill unavailable: nonexistent")

	h.requireInboxDrained(sessionID)

	msgs := h.parentMessages(sessionID)
	assert.False(t, hasUserContaining(msgs, "/skill nonexistent"), "a rejected command never enters the transcript")
	assert.Len(t, rec.snapshot(), callsBefore, "a rejected command must not cost a model turn")
	assert.False(t, h.mgr.HasActiveLoop(sessionID))
}

// The same rejection on a session with nothing in it yet. There is no settled
// assistant turn to end the activation, so a rejection that does not resolve the
// loop hands the provider a conversation that asks it nothing — an empty message
// list, or a project's AGENTS.md header on its own.
func TestHarnessScenario_UnknownSkillOnAFreshSessionCostsNoModelTurn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		claudeMD  string
		wantStart int
	}{
		{name: "empty project"},
		{name: "project with a CLAUDE.md header", claudeMD: "# House rules\nBe careful.", wantStart: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rec := newSkillHarness(t, nil, plainRespond)
			collector := collectEvents(h.mgr.PubSub().SubscribeAll())

			defer func() {
				collector.stop()
				h.shutdown()
			}()

			if tc.claudeMD != "" {
				require.NoError(t, os.WriteFile(
					filepath.Join(h.workDir(), "CLAUDE.md"), []byte(tc.claudeMD), 0o600,
				))
			}

			sessionID, err := h.mgr.Send(h.ctx, h.projectID, "/skill nonexistent", "fake-model", nil)
			require.NoError(t, err)

			collector.waitFor(t, "rejection notice", func(e []controllerapi.SessionNotification) bool {
				return len(warningNotices(e, sessionID)) > 0
			})
			h.mgr.waitIdle(sessionID)

			assert.Len(t, warningNotices(collector.snapshot(), sessionID), 1, "exactly one rejection notice")
			h.requireInboxDrained(sessionID)
			assert.Empty(t, rec.snapshot(), "a conversation that asks nothing is never sent to the provider")
			assert.Len(t, h.parentMessages(sessionID), tc.wantStart, "a rejected command writes nothing")
		})
	}
}
