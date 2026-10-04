package session

import (
	"strings"
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

func TestLoopDetectorTakesMinimumOfArgAndResultDiversity(t *testing.T) {
	tests := []struct {
		name string
		gen  func(i int) toolRecord
	}{
		{
			name: "one argument, every result different",
			gen:  func(i int) toolRecord { return toolRecord{name: "bash", argsHash: 1, resultHash: uint64(i)} },
		},
		{
			name: "every argument different, one result",
			gen:  func(i int) toolRecord { return toolRecord{name: "bash", argsHash: uint64(i), resultHash: 7} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ld := newLoopDetector()
			recordRounds(ld, loopDetectorMinFill, tt.gen)

			require.Len(t, ld.window, loopDetectorMinFill)
			require.False(t, ld.hasConsecutiveIdentical(), "fast path must not shadow the diversity check")

			// One dimension sits at 1.0 and the other at 0.1: the verdict must
			// follow the low one, otherwise half the loop shapes go unnoticed.
			assert.Equal(t, actionWarn, ld.check())
		})
	}
}

func TestLoopDetectorDiversityThresholdBoundary(t *testing.T) {
	tests := []struct {
		name   string
		unique int
		want   loopAction
	}{
		{name: "exactly at the threshold", unique: 7, want: actionNone},     // 7/20 = 0.35
		{name: "one step below the threshold", unique: 6, want: actionWarn}, // 6/20 = 0.30
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ld := newLoopDetector()
			recordRounds(ld, loopDetectorWindowSize, func(i int) toolRecord {
				k := uint64(i % tt.unique)

				return toolRecord{name: "t", argsHash: k, resultHash: k}
			})

			require.Len(t, ld.window, loopDetectorWindowSize)
			require.False(t, ld.hasConsecutiveIdentical(), "fast path must not shadow the diversity check")
			assert.Equal(t, tt.want, ld.check())
		})
	}
}

func TestLoopDetectorMinFillBoundary(t *testing.T) {
	gen := func(i int) toolRecord {
		k := uint64(i % 2)

		return toolRecord{name: "t", argsHash: k, resultHash: k}
	}

	tests := []struct {
		name   string
		rounds int
		want   loopAction
	}{
		{name: "one record short of the fill threshold", rounds: loopDetectorMinFill - 1, want: actionNone},
		{name: "exactly at the fill threshold", rounds: loopDetectorMinFill, want: actionWarn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ld := newLoopDetector()
			recordRounds(ld, tt.rounds, gen)

			require.False(t, ld.hasConsecutiveIdentical(), "fast path must not shadow the fill check")
			assert.Equal(t, tt.want, ld.check())
		})
	}
}

//nolint:intrange,modernize // Preserve the existing uint64 fixture loop during mechanical file consolidation.
func TestLoopDetectorEscalatesAtJaccardThreshold(t *testing.T) {
	rec := func(k uint64) toolRecord { return toolRecord{name: "t", argsHash: k, resultHash: k} }

	tests := []struct {
		name      string
		freshKeys uint64
		want      loopAction
	}{
		{name: "similarity exactly at the threshold", freshKeys: 3, want: actionBlock}, // 7/10 = 0.70
		{name: "similarity below the threshold", freshKeys: 4, want: actionWarn},       // 7/11 = 0.64
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ld := newLoopDetector()
			for k := uint64(0); k < 7; k++ {
				ld.record([]toolRecord{rec(k)})
			}

			// A trailing identical triple pins the fast path on for both checks,
			// so the second verdict depends only on the Jaccard comparison.
			ld.record([]toolRecord{rec(6)})
			ld.record([]toolRecord{rec(6)})
			require.Equal(t, actionWarn, ld.check())
			require.Len(t, ld.warnFingerprint, 7)

			last := 6 + tt.freshKeys
			for k := uint64(7); k <= last; k++ {
				ld.record([]toolRecord{rec(k)})
			}

			ld.record([]toolRecord{rec(last)})
			ld.record([]toolRecord{rec(last)})

			require.True(t, ld.hasConsecutiveIdentical())
			require.LessOrEqual(t, len(ld.window), loopDetectorWindowSize, "window must not evict warned keys")
			assert.Equal(t, tt.want, ld.check())
		})
	}
}

func TestLoopDetectorFailureStreakThresholds(t *testing.T) {
	fail := func(i int) toolRecord {
		return toolRecord{name: "edit", argsHash: uint64(i), resultHash: 42, failed: true}
	}

	tests := []struct {
		name   string
		rounds int
		want   loopAction
	}{
		{name: "one short of the warn threshold", rounds: loopDetectorFailWarn - 1, want: actionNone},
		{name: "exactly at the warn threshold", rounds: loopDetectorFailWarn, want: actionWarnFailure},
		{name: "one short of the block threshold", rounds: loopDetectorFailBlock - 1, want: actionWarnFailure},
		{name: "exactly at the block threshold", rounds: loopDetectorFailBlock, want: actionBlock},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ld := newLoopDetector()
			recordRounds(ld, tt.rounds, fail)

			assert.Equal(t, tt.want, ld.check())
		})
	}
}

func TestLoopDetectorBlockEscalationCounts(t *testing.T) {
	ld := newLoopDetector()
	rec := toolRecord{name: "edit", argsHash: 1, resultHash: 1}
	recordRounds(ld, loopDetectorMinFill, func(int) toolRecord { return rec })

	require.Equal(t, actionWarn, ld.check())

	// Exactly loopDetectorMaxBlocks blocks are handed out before the detector
	// gives up on tools entirely.
	for i := range loopDetectorMaxBlocks {
		assert.Equalf(t, actionBlock, ld.check(), "block %d", i+1)
	}

	assert.Equal(t, actionForceTextOnly, ld.check())
}

func TestLoopDetectorHealthyDiversityResetsEscalation(t *testing.T) {
	ld := newLoopDetector()
	rec := toolRecord{name: "edit", argsHash: 1, resultHash: 1}
	recordRounds(ld, loopDetectorMinFill, func(int) toolRecord { return rec })

	require.Equal(t, actionWarn, ld.check())

	// Fill the window with distinct work: the detector must forget the warning,
	// not carry it into a session that has clearly moved on.
	recordRounds(ld, loopDetectorWindowSize, func(i int) toolRecord {
		return toolRecord{name: "t", argsHash: uint64(i), resultHash: uint64(i)}
	})

	assert.Equal(t, actionNone, ld.check())
	assert.False(t, ld.warnActive)
	assert.Nil(t, ld.warnFingerprint)
	assert.False(t, ld.blocked)
	assert.Equal(t, 0, ld.consecutiveBlocks)
}

func TestLoopDetectorDedupKeepsLaterDistinctRecords(t *testing.T) {
	ld := newLoopDetector()
	first := toolRecord{name: "read", argsHash: 1, resultHash: 1}
	second := toolRecord{name: "grep", argsHash: 2, resultHash: 2}

	ld.record([]toolRecord{first, first, second, first})

	assert.Equal(t, []toolRecord{first, second}, ld.window)
}

func TestLoopDetectorTrimsOnlyAboveCapacity(t *testing.T) {
	ld := newLoopDetector()
	recordRounds(ld, loopDetectorWindowSize, func(i int) toolRecord {
		return toolRecord{name: "t", argsHash: uint64(i)}
	})

	require.Len(t, ld.window, loopDetectorWindowSize)
	assert.Equal(t, uint64(0), ld.window[0].argsHash, "a full window must not shed its oldest record")

	ld.record([]toolRecord{{name: "t", argsHash: 999}})

	require.Len(t, ld.window, loopDetectorWindowSize)
	assert.Equal(t, uint64(1), ld.window[0].argsHash)
	assert.Equal(t, uint64(999), ld.window[loopDetectorWindowSize-1].argsHash)
}

func TestLoopDetectorTrimsOversizedRound(t *testing.T) {
	ld := newLoopDetector()

	overflow := loopDetectorWindowSize + 5
	round := make([]toolRecord, 0, overflow)

	for i := range overflow {
		round = append(round, toolRecord{name: "t", argsHash: uint64(i)})
	}

	ld.record(round)

	require.Len(t, ld.window, loopDetectorWindowSize)
	assert.Equal(t, uint64(5), ld.window[0].argsHash)
}

func TestConsecutiveFailureStreak(t *testing.T) {
	fail := func(name string, args, result uint64) toolRecord {
		return toolRecord{name: name, argsHash: args, resultHash: result, failed: true}
	}
	ok := func(name string, args, result uint64) toolRecord {
		return toolRecord{name: name, argsHash: args, resultHash: result}
	}

	tests := []struct {
		name   string
		window []toolRecord
		want   int
	}{
		{name: "empty window", window: nil, want: 0},
		{name: "last record succeeded", window: []toolRecord{fail("edit", 1, 9), ok("edit", 2, 9)}, want: 0},
		{
			name:   "whole window is one streak",
			window: []toolRecord{fail("edit", 1, 9), fail("edit", 2, 9), fail("edit", 3, 9)},
			want:   3,
		},
		{
			name:   "different error breaks the streak",
			window: []toolRecord{fail("edit", 1, 9), fail("edit", 2, 8), fail("edit", 3, 9)},
			want:   1,
		},
		{
			name:   "different tool breaks the streak",
			window: []toolRecord{fail("edit", 1, 9), fail("bash", 2, 9), fail("edit", 3, 9)},
			want:   1,
		},
		{
			name:   "success breaks the streak mid-window",
			window: []toolRecord{fail("edit", 1, 9), ok("edit", 2, 9), fail("edit", 3, 9), fail("edit", 4, 9)},
			want:   2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ld := newLoopDetector()
			ld.window = tt.window

			assert.Equal(t, tt.want, ld.consecutiveFailureStreak())
		})
	}
}

func TestHasConsecutiveIdentical(t *testing.T) {
	a := toolRecord{name: "read", argsHash: 1, resultHash: 1}
	b := toolRecord{name: "read", argsHash: 2, resultHash: 1}

	tests := []struct {
		name   string
		window []toolRecord
		want   bool
	}{
		{name: "shorter than the trigger", window: []toolRecord{a, a}, want: false},
		{name: "exactly the trigger", window: []toolRecord{a, a, a}, want: true},
		{name: "oldest of the three differs", window: []toolRecord{b, a, a}, want: false},
		{name: "newest of the three differs", window: []toolRecord{a, a, b}, want: false},
		{name: "only the tail is identical", window: []toolRecord{b, b, a, a, a}, want: true},
		{
			name:   "failure flag is part of identity",
			window: []toolRecord{a, a, {name: "read", argsHash: 1, resultHash: 1, failed: true}},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ld := newLoopDetector()
			ld.window = tt.window

			assert.Equal(t, tt.want, ld.hasConsecutiveIdentical())
		})
	}
}

func TestFingerprintResultUsesHeadAndTailOnly(t *testing.T) {
	head := strings.Repeat("h", 256)
	tail := strings.Repeat("t", 256)
	base := head + strings.Repeat("a", 100) + tail

	// Same length, same first and last 256 bytes: the fingerprint is a
	// head+tail digest, so a differing middle must not register.
	assert.Equal(t, fingerprintResult(base), fingerprintResult(head+strings.Repeat("b", 100)+tail))

	assert.NotEqual(
		t,
		fingerprintResult(base),
		fingerprintResult(strings.Repeat("x", 256)+strings.Repeat("a", 100)+tail),
	)
	assert.NotEqual(
		t,
		fingerprintResult(base),
		fingerprintResult(head+strings.Repeat("a", 100)+strings.Repeat("x", 256)),
	)
	assert.NotEqual(t, fingerprintResult(base), fingerprintResult(head+strings.Repeat("a", 101)+tail))
}

func TestHeadBytes(t *testing.T) {
	assert.Equal(t, "ab", headBytes("abc", 2))
	assert.Equal(t, "abc", headBytes("abc", 3))
	assert.Equal(t, "abc", headBytes("abc", 4))
	assert.Empty(t, headBytes("", 0))
}

func TestTailBytes(t *testing.T) {
	assert.Equal(t, "bc", tailBytes("abc", 2))
	assert.Equal(t, "abc", tailBytes("abc", 3))
	assert.Equal(t, "abc", tailBytes("abc", 4))
	assert.Empty(t, tailBytes("", 0))
}

func TestCompactJSONFallsBackToRawInput(t *testing.T) {
	assert.Equal(t, `{"a":1}`, compactJSON([]byte(" { \"a\" : 1 } ")))
	assert.Equal(t, "not json", compactJSON([]byte("not json")))
	assert.NotEqual(t, fingerprintArgs([]byte("not json")), fingerprintArgs([]byte("also not json")))
}

func TestJaccardSimilarityArgs(t *testing.T) {
	set := func(hashes ...uint64) map[argKey]struct{} {
		s := make(map[argKey]struct{}, len(hashes))
		for _, h := range hashes {
			s[argKey{name: "t", argsHash: h}] = struct{}{}
		}

		return s
	}

	a := set(1, 2, 3)
	b := set(1, 4, 5)

	// One shared key, five distinct keys overall. Counting the union by
	// short-circuiting instead of skipping shared keys inflates this.
	for range mapOrderTrials {
		assert.InDelta(t, 0.2, jaccardSimilarityArgs(a, b), 1e-9)
	}

	assert.InDelta(t, 1.0, jaccardSimilarityArgs(a, a), 1e-9)
	assert.InDelta(t, 0.0, jaccardSimilarityArgs(set(), set()), 1e-9)
	assert.InDelta(t, 0.0, jaccardSimilarityArgs(a, set()), 1e-9)
	assert.InDelta(t, 0.0, jaccardSimilarityArgs(set(), a), 1e-9)
	assert.InDelta(t, 0.5, jaccardSimilarityArgs(set(1, 2), set(1, 2, 3, 4)), 1e-9)
}

func TestWindowAsArgSetIgnoresResultHash(t *testing.T) {
	window := []toolRecord{
		{name: "read", argsHash: 1, resultHash: 10},
		{name: "read", argsHash: 1, resultHash: 20},
		{name: "read", argsHash: 2, resultHash: 10},
	}

	assert.Equal(t, map[argKey]struct{}{
		{name: "read", argsHash: 1}: {},
		{name: "read", argsHash: 2}: {},
	}, windowAsArgSet(window))
}
