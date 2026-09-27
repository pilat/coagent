package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestClassifyResponseFacts(t *testing.T) {
	cases := []struct {
		name          string
		facts         responseFacts
		kind          sessionstore.ResponseDispositionKind
		output        string
		wantCandidate int64
		nudge         bool
	}{
		{
			name:  "candidate remains hidden",
			facts: responseFacts{response: textResponse("answer"), completionNudge: "check"},
			kind:  sessionstore.ResponseDispositionCandidate,
			nudge: true,
		},
		{
			name: "confirmation publishes candidate",
			facts: responseFacts{
				response:      textResponse("ack"),
				candidateID:   7,
				candidateText: "answer",
				outputEnabled: true,
				replyPending:  true,
			},
			kind:          sessionstore.ResponseDispositionConfirmed,
			output:        "answer",
			wantCandidate: 7,
		},
		{
			name:          "tools invalidate candidate",
			facts:         responseFacts{response: toolCallResponse("read-1", "read"), candidateID: 7},
			kind:          sessionstore.ResponseDispositionToolCall,
			wantCandidate: 7,
		},
		{
			name:          "empty cannot confirm",
			facts:         responseFacts{response: textResponse(" "), candidateID: 7},
			kind:          sessionstore.ResponseDispositionEmptyStop,
			wantCandidate: 7,
			nudge:         true,
		},
		{
			name:  "malformed tool finish is empty",
			facts: responseFacts{response: &llmwire.Response{Text: "not final", FinishType: llmwire.FinishToolCalls}},
			kind:  sessionstore.ResponseDispositionEmptyStop,
			nudge: true,
		},
		{
			name:   "sixth empty ends normally",
			facts:  responseFacts{response: textResponse(""), emptyStreak: 5},
			kind:   sessionstore.ResponseDispositionEmptyStop,
			output: sessionstore.EmptyStopTerminalNotice(6),
		},
		{
			name:  "wake skips empty escalation",
			facts: responseFacts{response: textResponse(""), wake: true, emptyStreak: 5},
			kind:  sessionstore.ResponseDispositionBackgroundYield,
		},
		{
			name: "projection failure retains attempt",
			facts: responseFacts{
				response:        textResponse("answer"),
				projectionError: projectionErrorNotice(errors.New("ledger unavailable")),
			},
			kind:   sessionstore.ResponseDispositionProjectionError,
			output: projectionErrorNotice(errors.New("ledger unavailable")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := classifyResponse(tc.facts)
			assert.Equal(t, tc.kind, decision.kind)
			assert.Equal(t, tc.output, decision.output)
			assert.Equal(t, tc.wantCandidate, decision.expectedCandidate)
			assert.Equal(t, tc.nudge, decision.nudge != nil)
		})
	}
}

func TestEmptyDispositionPreservesDirectReplyEligibility(t *testing.T) {
	_, db, _, sessionID, runner := newDispositionLoop(t)
	runner.lastResp = textResponse("")
	require.NoError(t, runner.recordIteration(t.Context()))
	assert.True(t, runner.directReplyEligible)
	assert.True(t, runner.replyToInput)
	runner.lastResp = toolCallResponse("read-1", "read")
	runner.lastResp.Text = "I am checking the file."
	require.NoError(t, runner.recordIteration(t.Context()))
	rows := outboxRows(t, db, sessionID)
	require.Len(t, rows, 1)
	assert.Equal(t, "I am checking the file.", rows[0]["content"])
	assert.Equal(t, false, rows[0]["releases"])
}
