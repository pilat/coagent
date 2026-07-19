package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoopDetector_DiversityCalculation(t *testing.T) {
	ld := newLoopDetector()

	for i := range loopDetectorMinFill {
		ld.record([]toolRecord{{name: "tool", argsHash: uint64(i), resultHash: uint64(i)}})
	}

	action := ld.check()
	assert.Equal(t, actionNone, action)
}

func TestLoopDetector_FastPathConsecutive(t *testing.T) {
	ld := newLoopDetector()

	rec := toolRecord{name: "grep", argsHash: 100, resultHash: 200}
	for range loopDetectorConsecutiveWarn {
		ld.record([]toolRecord{rec})
	}

	action := ld.check()
	assert.Equal(t, actionWarn, action)
}

func TestLoopDetector_EscalationSequence(t *testing.T) {
	ld := newLoopDetector()

	rec := toolRecord{name: "edit", argsHash: 1, resultHash: 1}

	for range loopDetectorMinFill {
		ld.record([]toolRecord{rec})
	}
	action := ld.check()
	assert.Equal(t, actionWarn, action)

	action = ld.check()
	assert.Equal(t, actionBlock, action)

	action = ld.check()
	assert.Equal(t, actionBlock, action)

	action = ld.check()
	assert.Equal(t, actionBlock, action)

	action = ld.check()
	assert.Equal(t, actionForceTextOnly, action)

	action = ld.check()
	assert.Equal(t, actionForceTextOnly, action)
}

func TestLoopDetector_ResetWindow(t *testing.T) {
	ld := newLoopDetector()

	rec := toolRecord{name: "bash", argsHash: 1, resultHash: 1}
	for range loopDetectorMinFill {
		ld.record([]toolRecord{rec})
	}

	ld.check()
	assert.True(t, ld.warnActive)

	ld.resetWindow()

	assert.Empty(t, ld.window)
	assert.False(t, ld.warnActive)
	assert.Nil(t, ld.warnFingerprint)
	assert.False(t, ld.blocked)
	assert.Equal(t, 0, ld.consecutiveBlocks)
	assert.False(t, ld.forceTextOnly)
}

func TestLoopDetector_DedupWithinRound(t *testing.T) {
	ld := newLoopDetector()

	rec := toolRecord{name: "read", argsHash: 42, resultHash: 99}
	ld.record([]toolRecord{rec, rec, rec, rec, rec})

	assert.Len(t, ld.window, 1)
}

func TestLoopDetector_WindowCapacityOverflow(t *testing.T) {
	ld := newLoopDetector()

	for i := range loopDetectorWindowSize + 5 {
		ld.record([]toolRecord{{name: "tool", argsHash: uint64(i), resultHash: uint64(i)}})
	}

	assert.Len(t, ld.window, loopDetectorWindowSize)
	assert.Equal(t, uint64(5), ld.window[0].argsHash)
}

func TestFingerprintResult(t *testing.T) {
	h1 := fingerprintResult("hello world")
	h2 := fingerprintResult("hello world")
	assert.Equal(t, h1, h2)

	h3 := fingerprintResult("different content")
	assert.NotEqual(t, h1, h3)
}

func TestFingerprintArgs(t *testing.T) {
	h1 := fingerprintArgs([]byte(`{"a": "b"}`))
	h2 := fingerprintArgs([]byte(`{"a":"b"}`))
	assert.Equal(t, h1, h2)

	h3 := fingerprintArgs([]byte(`{"a":"c"}`))
	assert.NotEqual(t, h1, h3)
}

func TestLoopDetector_ClearForceTextOnly(t *testing.T) {
	ld := newLoopDetector()

	rec := toolRecord{name: "bash", argsHash: 1, resultHash: 1}
	for range loopDetectorMinFill {
		ld.record([]toolRecord{rec})
	}

	ld.check() // warn
	ld.check() // block (cb=0)
	ld.check() // block (cb=1)
	ld.check() // block (cb=2)
	action := ld.check()
	require.Equal(t, actionForceTextOnly, action)

	ld.clearForceTextOnly()
	assert.Empty(t, ld.window) // recovery starts from a clean window

	// A fresh streak still escalates — the warn cycle restarts from scratch.
	for range loopDetectorConsecutiveWarn {
		ld.record([]toolRecord{rec})
	}
	action = ld.check()
	assert.Equal(t, actionWarn, action)
}

func TestLoopDetector_RepeatedFailure(t *testing.T) {
	ld := newLoopDetector()

	// Same tool, same error, but wobbling args (argsHash differs each round) —
	// the exact pattern that slips past the arg-diversity paths.
	fail := func(i int) toolRecord {
		return toolRecord{name: "edit", argsHash: uint64(i), resultHash: 42, failed: true}
	}

	for i := range loopDetectorFailWarn - 1 {
		ld.record([]toolRecord{fail(i)})
		assert.Equal(t, actionNone, ld.check())
	}

	ld.record([]toolRecord{fail(100)})
	assert.Equal(t, actionWarnFailure, ld.check())

	for i := loopDetectorFailWarn; i < loopDetectorFailBlock; i++ {
		ld.record([]toolRecord{fail(i)})
	}
	assert.Equal(t, actionBlock, ld.check())
}

func TestLoopDetector_FailureStreakBrokenBySuccess(t *testing.T) {
	ld := newLoopDetector()

	for i := range loopDetectorFailBlock {
		ld.record([]toolRecord{{name: "edit", argsHash: uint64(i), resultHash: 42, failed: true}})
	}

	// A single successful call breaks the streak — no more failure escalation.
	ld.record([]toolRecord{{name: "edit", argsHash: 999, resultHash: 7, failed: false}})
	assert.Equal(t, 0, ld.consecutiveFailureStreak())
}

func TestLoopDetector_PingPongLoop(t *testing.T) {
	ld := newLoopDetector()

	recA := toolRecord{name: "edit", argsHash: 1, resultHash: 10}
	recB := toolRecord{name: "read", argsHash: 2, resultHash: 20}

	for range loopDetectorMinFill / 2 {
		ld.record([]toolRecord{recA})
		ld.record([]toolRecord{recB})
	}

	action := ld.check()
	assert.Equal(t, actionWarn, action)
}
