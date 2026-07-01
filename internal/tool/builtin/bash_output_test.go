package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestBashTimeoutClamping(t *testing.T) {
	tests := []struct {
		name string
		ms   int
		want time.Duration
	}{
		{name: "zero falls back to the default", ms: 0, want: defaultBashTimeout},
		{name: "negative falls back to the default", ms: -1, want: defaultBashTimeout},
		{name: "one millisecond is honoured", ms: 1, want: time.Millisecond},
		{
			name: "just under the cap is honoured",
			ms:   int(maxBashTimeout/time.Millisecond) - 1,
			want: maxBashTimeout - time.Millisecond,
		},
		{name: "exactly the cap is honoured", ms: int(maxBashTimeout / time.Millisecond), want: maxBashTimeout},
		{name: "over the cap is clamped", ms: int(maxBashTimeout/time.Millisecond) + 1, want: maxBashTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, bashTimeout(tt.ms))
		})
	}
}

func TestCombineOutputStreams(t *testing.T) {
	tests := []struct {
		name          string
		stdout        string
		stderr        string
		want          string
		wantTruncated bool
	}{
		{name: "both empty", want: ""},
		{name: "stdout only", stdout: "out\n", want: "out\n"},
		{name: "stderr only", stderr: "err\n", want: "[stderr]\nerr\n"},
		{name: "both streams separated", stdout: "out", stderr: "err", want: "out\n[stderr]\nerr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := combineOutput(bytes.NewBufferString(tt.stdout), bytes.NewBufferString(tt.stderr))

			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantTruncated, truncated)
		})
	}
}

func TestCombineOutputTruncationBoundary(t *testing.T) {
	tests := []struct {
		name      string
		size      int
		truncated bool
	}{
		{name: "exactly at the cap is kept whole", size: maxOutputSize, truncated: false},
		{name: "one byte over is cut", size: maxOutputSize + 1, truncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := strings.Repeat("z", tt.size)

			got, truncated := combineOutput(bytes.NewBufferString(payload), &bytes.Buffer{})

			assert.Equal(t, tt.truncated, truncated)

			if tt.truncated {
				assert.Equal(t, strings.Repeat("z", maxOutputSize)+"\n\n(Output truncated)", got)
				return
			}

			assert.Equal(t, payload, got)
		})
	}
}

func TestBashToolTitleTruncationBoundary(t *testing.T) {
	tests := []struct {
		name      string
		commandLn int
		wantLn    int
		wantCut   bool
	}{
		{name: "exactly fifty characters is kept whole", commandLn: 50, wantLn: 50},
		{name: "fifty one characters is cut", commandLn: 51, wantLn: 50, wantCut: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := "true #" + strings.Repeat("c", tt.commandLn-len("true #"))
			result := runBash(t, bashParams{Command: command})

			assert.Equal(t, tt.wantCut, strings.HasSuffix(result.Title, "..."))
			assert.Len(t, strings.TrimSuffix(result.Title, "..."), tt.wantLn)
		})
	}
}

func TestBashToolReportsExitCodeAndPlaceholderOutput(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		wantCode int
		wantOut  string
	}{
		{name: "silent success", command: "true", wantCode: 0, wantOut: noOutput},
		{name: "silent failure", command: "exit 3", wantCode: 3, wantOut: noOutput},
		{name: "failure with output", command: "echo partial; exit 4", wantCode: 4, wantOut: "partial"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := runBash(t, bashParams{Command: tt.command})

			assert.Equal(t, tt.wantOut, result.Output)
			assert.Equal(t, tt.wantCode, result.Metadata[metaKeyExitCode])
			assert.Equal(t, false, result.Metadata[metaKeyTimedOut])
		})
	}
}

func TestBashToolTimeoutMetadata(t *testing.T) {
	result := runBash(t, bashParams{Command: "sleep 5", Timeout: 100})

	assert.Equal(t, -1, result.Metadata[metaKeyExitCode])
	assert.Equal(t, true, result.Metadata[metaKeyTimedOut])
	assert.Contains(t, result.Output, "(Command timed out after 100ms)")
}

func TestBashToolRejectsBadInput(t *testing.T) {
	bashTool := newBashTool(t.TempDir(), &bashRunnerStub{})

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "malformed json", raw: `{`, wantErr: "invalid parameters"},
		{name: "empty command", raw: `{"command":""}`, wantErr: "command is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := bashTool.Execute(context.Background(), json.RawMessage(tt.raw))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func runBash(t *testing.T, p bashParams) *tool.Result {
	t.Helper()

	raw, err := json.Marshal(p)
	require.NoError(t, err)

	result, err := newBashTool(t.TempDir(), &bashRunnerStub{}).Execute(context.Background(), raw)
	require.NoError(t, err)

	return result
}
