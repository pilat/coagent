package daemon

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

// waitTimeLayout is deliberately a copy of the layout sessionevent renders wake
// times with: a change there must break the golden, not be normalized away.
const (
	waitTimeLayout     = "15:04 02 Jan"
	wakePlaceholder    = "<wake>"
	workDirPlaceholder = "<workdir>"
	// The session name embeds the temp project directory, which carries no
	// scenario meaning and changes with unrelated harness edits.
	namePlaceholder = "<name>"
)

// updateHarnessTraces rewrites the recorded goldens instead of asserting them:
// go test ./internal/daemon -run TestHarnessScenario -update-traces
var updateHarnessTraces = flag.Bool(
	"update-traces", false, "rewrite the recorded controller traces under internal/testdata",
)

var childRefPattern = regexp.MustCompile(`#(\d+)`)

// elapsedPattern matches the wall-time fragment of an automatic card; its
// value depends on scheduling and carries no conversation meaning.
var elapsedPattern = regexp.MustCompile(`⏱ [0-9.]+[a-z0-9.]+`)

// harnessTraceFile is the shared artifact: the exact ordered notification trace
// a daemon scenario published to a controller sink, plus the exact sanitized
// controllerapi.OutputClaimData sequence the production controller produced for
// the same run. The telegram manager scenario replays these claims through the
// production output transport. Ids are normalized; generations are kept.
type harnessTraceFile struct {
	SourceTest string              `json:"source_test"`
	Trace      []harnessTraceEvent `json:"trace"`
	Claims     []harnessTraceClaim `json:"claims,omitempty"`
}

// messagePlaceholderPrefix marks sanitized Telegram message receipts. The
// telegram replay maps each placeholder to a real message id, so edit decisions
// and chunk bookkeeping run against genuine targets.
const messagePlaceholderPrefix = "<msg-"

// harnessTraceClaim is the sanitized OutputClaimData one production claim
// produced. Presence-bearing generations stay exact; random attempt ids and raw
// message receipts do not survive sanitization.
type harnessTraceClaim struct {
	Type                         string         `json:"type"`
	Content                      string         `json:"content"`
	Attributes                   map[string]any `json:"attributes,omitempty"`
	SourceKey                    string         `json:"source_key,omitempty"`
	ModelInputGeneration         *int64         `json:"model_input_generation,omitempty"`
	PreviousMessageType          string         `json:"previous_message_type,omitempty"`
	PreviousModelInputGeneration *int64         `json:"previous_model_input_generation,omitempty"`
	PreviousMessageIDs           []string       `json:"previous_message_ids,omitempty"`
	ReleasesInput                bool           `json:"releases_input"`
}

type harnessTraceEvent struct {
	Type       string             `json:"type"`
	Message    string             `json:"message,omitempty"`
	Status     string             `json:"status,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	Source     string             `json:"source,omitempty"`
	Name       string             `json:"name,omitempty"`
	WorkDir    string             `json:"work_dir,omitempty"`
	Attributes map[string]any     `json:"attributes,omitempty"`
	Waiting    []harnessTraceWait `json:"waiting,omitempty"`
}

type harnessTraceWait struct {
	Kind  string `json:"kind"`
	Child string `json:"child,omitempty"`
	Wake  string `json:"wake,omitempty"`
}

func assertHarnessTrace(
	t *testing.T,
	name string,
	events []controllerapi.SessionNotification,
	sessionID int64,
) {
	t.Helper()

	got := harnessTraceFile{SourceTest: t.Name(), Trace: normalizeHarnessTrace(t, events, sessionID)}
	path := harnessTracePath(name)

	if *updateHarnessTraces {
		stored := readHarnessTraceFileForUpdate(t, path)
		stored.SourceTest = t.Name()
		stored.Trace = got.Trace
		writeHarnessTrace(t, path, stored)

		return
	}

	want := readHarnessTraceFile(t, path)
	assert.Equal(t, want.SourceTest, got.SourceTest, "golden %s belongs to another scenario", name)
	assert.Equal(t, want.Trace, got.Trace, "controller-visible notification trace")
}

// harnessTracePath resolves the shared fixture root. Both this package and
// internal/managers/telegram read the same files, so neither half can drift.
func harnessTracePath(name string) string {
	return filepath.Join("..", "testdata", "harness_scenarios", name)
}

func readHarnessTraceFile(t *testing.T, path string) harnessTraceFile {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err, "missing recorded trace; regenerate with -update-traces")

	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()

	var file harnessTraceFile
	require.NoError(t, decoder.Decode(&file))
	require.NotEmpty(t, file.Trace)

	return file
}

// readHarnessTraceFileForUpdate loads a trace file during recording, tolerating
// a file that only the notification pass has written so far.
func readHarnessTraceFileForUpdate(t *testing.T, path string) harnessTraceFile {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err, "run the notification trace recording before claim recording")

	var file harnessTraceFile
	require.NoError(t, json.Unmarshal(data, &file))

	return file
}

func writeHarnessTrace(t *testing.T, path string, file harnessTraceFile) {
	t.Helper()

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	require.NoError(t, encoder.Encode(file))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	t.Logf("recorded controller trace %s", path)
}

func normalizeHarnessTrace(
	t *testing.T,
	events []controllerapi.SessionNotification,
	sessionID int64,
) []harnessTraceEvent {
	t.Helper()

	children := map[int64]string{}
	wakes := []string{}
	out := make([]harnessTraceEvent, 0, len(events))

	for _, event := range events {
		if event.SessionID != sessionID {
			continue
		}

		n := event.Notification
		requireRecordableNotification(t, n)
		recorded := harnessTraceEvent{
			Type:   string(n.Type),
			Status: string(n.Status),
			Reason: n.Reason,
			Source: n.Source,
		}
		if len(n.Attributes) > 0 {
			recorded.Attributes = n.Attributes
		}
		if n.Name != "" {
			recorded.Name = namePlaceholder
		}
		if n.WorkDir != "" {
			recorded.WorkDir = workDirPlaceholder
		}

		for _, item := range n.Waiting {
			wait := harnessTraceWait{Kind: string(item.Kind)}
			switch item.Kind {
			case sessionevent.WaitSubagent:
				wait.Child = childRef(children, item.ChildID)
			case sessionevent.WaitSleep:
				wait.Wake = wakePlaceholder
				wakes = append(wakes, item.WakeAt.Local().Format(waitTimeLayout))
			}
			recorded.Waiting = append(recorded.Waiting, wait)
		}

		recorded.Message = normalizeHarnessMessage(n.Message, children, wakes)
		out = append(out, recorded)
	}

	return out
}

// requireRecordableNotification fails loudly instead of silently dropping a
// payload the shared trace schema cannot carry to the manager half.
func requireRecordableNotification(t *testing.T, n sessionevent.Notification) {
	t.Helper()

	require.Empty(t, n.RequestID, "extend the trace schema before recording secret requests")
	require.Zero(t, n.OldSessionID, "extend the trace schema before recording session clears")
	require.Zero(t, n.NewSessionID, "extend the trace schema before recording session clears")
}

func childRef(children map[int64]string, id int64) string {
	if ref, ok := children[id]; ok {
		return ref
	}

	ref := fmt.Sprintf("#%d", len(children)+1)
	children[id] = ref

	return ref
}

func normalizeHarnessMessage(message string, children map[int64]string, wakes []string) string {
	if message == "" {
		return ""
	}

	message = elapsedPattern.ReplaceAllString(message, "⏱ <elapsed>")

	for _, wake := range wakes {
		message = strings.ReplaceAll(message, wake, wakePlaceholder)
	}

	return childRefPattern.ReplaceAllStringFunc(message, func(match string) string {
		for id, ref := range children {
			if match == fmt.Sprintf("#%d", id) {
				return ref
			}
		}

		return match
	})
}
