//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/ctl"
	managercli "github.com/pilat/coagent/internal/managers/cli"
	"github.com/pilat/coagent/internal/migrate"
)

func TestHarnessE2E_SecondInputDoesNotReplayPreviousFinal(t *testing.T) {
	modelServer := newHarnessModelServer(t)
	home, err := os.MkdirTemp("/tmp", "coa-harness")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	writeHarnessConfig(t, home, modelServer.URL)
	binary := buildBinary(t)
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.SocketFileName)
	process := startDaemon(t, binary, home)
	defer func() { _ = process.Process.Kill() }()

	waitForStatus(t, socket, func(status ctl.StatusResult) bool {
		return status.ConfigPresent && status.ModelCount == 1
	})
	client, err := ctl.Dial(t.Context(), socket)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	var opened managercli.OpenResult
	require.NoError(t, client.Call(t.Context(), managercli.OpChatOpen, struct{}{}, &opened))

	firstSession := sendHarnessChat(t, client, managercli.SendParams{Text: "first question"})
	firstAnswer := "first answer"
	firstTrace := waitForHarnessChatMessage(t, client, firstSession.SessionID, firstAnswer)
	assert.Equal(t, []string{firstAnswer}, firstTrace)

	secondSession := sendHarnessChat(t, client, managercli.SendParams{
		SessionID: firstSession.SessionID,
		Text:      "second question",
	})
	require.Equal(t, firstSession.SessionID, secondSession.SessionID)
	secondAnswer := "second answer"
	secondTrace := waitForHarnessChatMessage(t, client, secondSession.SessionID, secondAnswer)
	require.Len(t, secondTrace, 1)
	assert.Equal(t, secondAnswer, secondTrace[0],
		"the compiled daemon must not replay history as a new controller event")
}

func TestHarnessE2E_ForegroundFollowUpRejectsCompetingSleep(t *testing.T) {
	modelServer, releaseInitial, releaseFollowUp := newHarnessChildModelServer(t)
	defer closeHarnessChannel(releaseInitial)
	defer closeHarnessChannel(releaseFollowUp)
	home, err := os.MkdirTemp("/tmp", "coa-harness")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	writeHarnessConfig(t, home, modelServer.URL)
	binary := buildBinary(t)
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.SocketFileName)
	process, daemonLog := startHarnessDaemon(t, binary, home)
	defer func() { _ = process.Process.Kill() }()

	waitForStatus(t, socket, func(status ctl.StatusResult) bool {
		return status.ConfigPresent && status.ModelCount == 1
	})
	client, err := ctl.Dial(t.Context(), socket)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, client.Call(t.Context(), managercli.OpChatOpen, struct{}{}, &managercli.OpenResult{}))

	started := sendHarnessChat(t, client, managercli.SendParams{Text: "start foreground child"})
	waitForHarnessEvent(t, client, started.SessionID, "foreground child wait", func(event managercli.Event) bool {
		return event.Type == "waiting" ||
			(event.Type == "message" && strings.Contains(event.Message, "⏳ Waiting on 1 item"))
	})
	close(releaseInitial)
	initialAnswer := "initial child delivered"
	initial := waitForHarnessChatTrace(t, client, started.SessionID, initialAnswer)
	assert.Contains(t, initial.Messages, initialAnswer)
	sendHarnessChat(t, client, managercli.SendParams{
		SessionID: started.SessionID,
		Text:      "continue the same child",
	})
	acceptedAnswer := "follow-up accepted"
	accepted := waitForHarnessChatTrace(t, client, started.SessionID, acceptedAnswer)
	assert.Equal(t, []string{acceptedAnswer}, accepted.Messages)
	assert.Zero(t, accepted.Waiting,
		"send_to_subagent+sleep must be rejected before a competing wait reaches a controller")
	assert.Zero(t, accepted.Errors, "parent/child concurrency must not leak transient SQLite errors")

	close(releaseFollowUp)
	continuedAnswer := "continuation delivered"
	continued := waitForHarnessChatTrace(t, client, started.SessionID, continuedAnswer)
	assert.Contains(t, continued.Messages, continuedAnswer)
	assert.Zero(t, continued.Waiting)
	assert.Zero(t, continued.Errors)
	assertHarnessDaemonHasNoSQLiteContention(t, daemonLog)
}

func TestHarnessE2E_RestartReplaysCommittedOutputToReconnectedCLI(t *testing.T) {
	modelServer, release := newRestartHarnessModelServer(t)
	defer closeHarnessChannel(release)
	home, err := os.MkdirTemp("/tmp", "coa-harness")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	writeHarnessConfig(t, home, modelServer.URL)
	binary := buildBinary(t)
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.SocketFileName)
	first := startDaemon(t, binary, home)
	waitForStatus(t, socket, func(status ctl.StatusResult) bool { return status.ConfigPresent })

	client, err := ctl.Dial(t.Context(), socket)
	require.NoError(t, err)
	require.NoError(t, client.Call(t.Context(), managercli.OpChatOpen, struct{}{}, &managercli.OpenResult{}))
	session := sendHarnessChat(t, client, managercli.SendParams{Text: "restart question"})
	require.NoError(t, client.Close(), "the first terminal must not acknowledge the answer")
	close(release)
	waitForCommittedHarnessOutput(
		t,
		filepath.Join(home, coagenthome.DirName, coagenthome.DBFileName),
		session.SessionID,
		"restart answer",
	)
	require.NoError(t, first.Process.Kill())
	require.Error(t, first.Wait())

	second := startDaemon(t, binary, home)
	defer func() { _ = second.Process.Kill() }()
	waitForStatus(t, socket, func(status ctl.StatusResult) bool { return status.ConfigPresent })
	reconnected, err := ctl.Dial(t.Context(), socket)
	require.NoError(t, err)
	defer func() { _ = reconnected.Close() }()
	require.NoError(t, reconnected.Call(t.Context(), managercli.OpChatOpen, struct{}{}, &managercli.OpenResult{}))
	restartAnswer := "restart answer"
	trace := waitForHarnessChatTrace(t, reconnected, session.SessionID, restartAnswer)
	assert.Equal(t, []string{restartAnswer}, trace.Messages)
}

func newHarnessModelServer(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")

		switch request.URL.Path {
		case "/models":
			_, _ = response.Write([]byte(`{
				"data":[{
					"id":"fake/model",
					"name":"Harness Model",
					"context_length":200000,
					"top_provider":{"context_length":200000,"max_completion_tokens":8192},
					"pricing":{"prompt":"0","completion":"0"}
				}]
			}`))
		case "/chat/completions":
			var body struct {
				Messages []struct {
					Role    string `json:"role"`
					Content any    `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode harness model request: %v", err)
				http.Error(response, "invalid request", http.StatusBadRequest)

				return
			}

			answer := "first answer"
			for _, message := range body.Messages {
				if message.Role == "user" && strings.Contains(fmt.Sprint(message.Content), "second question") {
					answer = "second answer"
				}
			}

			payload, err := json.Marshal(map[string]any{
				"id": "completion", "model": "fake/model",
				"choices": []any{map[string]any{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": answer},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{
					"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2,
				},
			})
			if err != nil {
				t.Errorf("encode harness model response: %v", err)
				http.Error(response, "encode response", http.StatusInternalServerError)

				return
			}
			_, _ = response.Write(payload)
		default:
			http.NotFound(response, request)
		}
	}))
}

func newRestartHarnessModelServer(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/models" {
			writeHarnessCatalog(response)
			return
		}
		if request.URL.Path != "/chat/completions" {
			http.NotFound(response, request)
			return
		}
		var body harnessModelRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode restart harness request: %v", err)
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		if body.hasUser("restart question") {
			<-release
			writeHarnessTextCompletion(t, response, "restart answer")
			return
		}
		writeHarnessTextCompletion(t, response, "unexpected")
	}))

	return server, release
}

func newHarnessChildModelServer(t *testing.T) (*httptest.Server, chan struct{}, chan struct{}) {
	t.Helper()

	releaseInitial := make(chan struct{})
	releaseFollowUp := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/models" {
			writeHarnessCatalog(response)

			return
		}
		if request.URL.Path != "/chat/completions" {
			http.NotFound(response, request)

			return
		}

		var body harnessModelRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode harness subagent request: %v", err)
			http.Error(response, "invalid request", http.StatusBadRequest)

			return
		}

		switch {
		case body.hasUser("CHILD_INITIAL"):
			if body.hasUser("FOLLOW_UP") {
				<-releaseFollowUp
				writeHarnessTextCompletion(t, response, "child continuation answer")
			} else {
				<-releaseInitial
				writeHarnessTextCompletion(t, response, "child initial answer")
			}
		case body.hasToolResult("subagent_event"):
			writeHarnessTextCompletion(t, response, "continuation delivered")
		case body.hasUser("continue the same child"):
			if body.hasToolResult("send_to_subagent") {
				writeHarnessTextCompletion(t, response, "follow-up accepted")

				return
			}

			childID, ok := body.childID()
			if !ok {
				t.Error("foreground task result did not contain a child id")
				http.Error(response, "missing child id", http.StatusInternalServerError)

				return
			}
			writeHarnessToolCompletion(t, response, []harnessToolCall{
				{
					ID: "follow-up-e2e", Name: "send_to_subagent",
					Arguments: fmt.Sprintf(`{"id":%d,"message":"FOLLOW_UP one more thing"}`, childID),
				},
				{
					ID: "sleep-e2e", Name: "sleep",
					Arguments: `{"duration":"1h","reason":"wait for follow-up"}`,
				},
			})
		case body.hasToolResult("task"):
			writeHarnessTextCompletion(t, response, "initial child delivered")
		default:
			writeHarnessToolCompletion(t, response, []harnessToolCall{{
				ID: "task-e2e", Name: "task",
				Arguments: `{"prompt":"CHILD_INITIAL","description":"e2e","subagent_type":"general"}`,
			}})
		}
	}))

	return server, releaseInitial, releaseFollowUp
}

type harnessModelRequest struct {
	Messages []harnessModelMessage `json:"messages"`
}

type harnessModelMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	Name    string `json:"name"`
}

func (r harnessModelRequest) hasUser(needle string) bool {
	for _, message := range r.Messages {
		if message.Role == "user" && strings.Contains(fmt.Sprint(message.Content), needle) {
			return true
		}
	}

	return false
}

func (r harnessModelRequest) hasToolResult(name string) bool {
	for _, message := range r.Messages {
		if message.Role == "tool" && message.Name == name {
			return true
		}
	}

	return false
}

var harnessChildIDPattern = regexp.MustCompile(`Subagent #([0-9]+)`)

func (r harnessModelRequest) childID() (int64, bool) {
	for _, message := range r.Messages {
		if message.Role != "tool" || message.Name != "task" {
			continue
		}

		match := harnessChildIDPattern.FindStringSubmatch(fmt.Sprint(message.Content))
		if len(match) != 2 {
			continue
		}
		id, err := strconv.ParseInt(match[1], 10, 64)

		return id, err == nil
	}

	return 0, false
}

type harnessToolCall struct {
	ID        string
	Name      string
	Arguments string
}

func writeHarnessCatalog(response http.ResponseWriter) {
	_, _ = response.Write([]byte(`{
		"data":[{
			"id":"fake/model",
			"name":"Harness Model",
			"context_length":200000,
			"top_provider":{"context_length":200000,"max_completion_tokens":8192},
			"pricing":{"prompt":"0","completion":"0"}
		}]
	}`))
}

func writeHarnessTextCompletion(t *testing.T, response http.ResponseWriter, text string) {
	t.Helper()
	writeHarnessCompletion(t, response, map[string]any{
		"role": "assistant", "content": text,
	}, "stop")
}

func writeHarnessToolCompletion(t *testing.T, response http.ResponseWriter, calls []harnessToolCall) {
	t.Helper()
	toolCalls := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		toolCalls = append(toolCalls, map[string]any{
			"id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
		})
	}
	writeHarnessCompletion(t, response, map[string]any{
		"role": "assistant", "content": nil, "tool_calls": toolCalls,
	}, "tool_calls")
}

func writeHarnessCompletion(
	t *testing.T,
	response http.ResponseWriter,
	message map[string]any,
	finishReason string,
) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"id": "completion", "model": "fake/model",
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2,
		},
	})
	if err != nil {
		t.Errorf("encode harness completion: %v", err)
		http.Error(response, "encode response", http.StatusInternalServerError)

		return
	}
	_, _ = response.Write(payload)
}

func writeHarnessConfig(t *testing.T, home, baseURL string) {
	t.Helper()

	dir := filepath.Join(home, coagenthome.DirName)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	configBody := fmt.Sprintf(`providers:
  fake:
    driver: openrouter
    api_key: test-key
    base_url: %s
models:
  - id: fake/model
    provider: fake
`, baseURL)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, coagenthome.ConfigFileName), []byte(configBody), 0o600,
	))
}

func sendHarnessChat(
	t *testing.T,
	client *ctl.Client,
	params managercli.SendParams,
) managercli.SendResult {
	t.Helper()

	var result managercli.SendResult
	require.NoError(t, client.Call(t.Context(), managercli.OpChatSend, params, &result))

	return result
}

type harnessChatTrace struct {
	Messages []string
	Waiting  int
	Errors   int
}

func waitForHarnessChatMessage(
	t *testing.T,
	client *ctl.Client,
	sessionID int64,
	target string,
) []string {
	t.Helper()

	return waitForHarnessChatTrace(t, client, sessionID, target).Messages
}

func waitForHarnessEvent(
	t *testing.T,
	client *ctl.Client,
	sessionID int64,
	label string,
	match func(managercli.Event) bool,
) {
	t.Helper()

	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()

	for {
		select {
		case notification, ok := <-client.Notifications():
			require.True(t, ok, "daemon disconnected before %s", label)
			if notification.Method != managercli.EventMethod {
				continue
			}

			var event managercli.Event
			require.NoError(t, json.Unmarshal(notification.Params, &event))
			if event.SessionID == sessionID && match(event) {
				return
			}
		case <-timer.C:
			t.Fatalf("session %d did not emit %s", sessionID, label)
		}
	}
}

func waitForHarnessChatTrace(
	t *testing.T,
	client *ctl.Client,
	sessionID int64,
	target string,
) harnessChatTrace {
	t.Helper()

	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	var trace harnessChatTrace
	targetSeen := false

	for {
		select {
		case notification, ok := <-client.Notifications():
			require.True(t, ok, "daemon disconnected before the session became idle")
			if notification.Method != managercli.EventMethod {
				continue
			}

			var event managercli.Event
			require.NoError(t, json.Unmarshal(notification.Params, &event))
			if event.SessionID != sessionID {
				continue
			}

			if event.Type == "message" {
				trace.Messages = append(trace.Messages, event.Message)
				if event.Message == target {
					targetSeen = true
				}
			}
			if event.Type == "waiting" {
				trace.Waiting++
			}
			if event.Type == "state_changed" && event.Status == "error" {
				trace.Errors++
			}
			if targetSeen && event.Type == "state_changed" && event.Status == "idle" {
				return trace
			}
		case <-timer.C:
			t.Fatalf("session %d did not become idle; trace: %+v", sessionID, trace)
		}
	}
}

func closeHarnessChannel(channel chan struct{}) {
	select {
	case <-channel:
	default:
		close(channel)
	}
}

func waitForCommittedHarnessOutput(t *testing.T, path string, sessionID int64, content string) {
	t.Helper()
	db, err := migrate.OpenDB(t.Context(), path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		var committed bool
		err := db.QueryRowContext(t.Context(), `SELECT EXISTS(
			SELECT 1 FROM sessions JOIN session_outbox ON session_outbox.session_id = sessions.id
			WHERE sessions.id = ? AND sessions.status = 'completed'
				AND session_outbox.content = ? AND session_outbox.state <> 'delivered'
		)`, sessionID, content).Scan(&committed)
		require.NoError(t, err)
		if committed {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("session %d did not commit undelivered %q", sessionID, content)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func startHarnessDaemon(t *testing.T, binary, home string) (*exec.Cmd, string) {
	t.Helper()

	logPath := filepath.Join(t.TempDir(), "daemon.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	process := exec.Command(binary, "daemon")
	process.Env = isolatedProcessEnv(os.Environ(), home)
	process.Stdout = logFile
	process.Stderr = logFile
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
		_ = logFile.Close()
	})

	return process, logPath
}

func assertHarnessDaemonHasNoSQLiteContention(t *testing.T, logPath string) {
	t.Helper()

	logBody, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.NotContains(t, string(logBody), "SQLITE_BUSY",
		"normal parent/child concurrency must not fail and rely on runner retries")
}
