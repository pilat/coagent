package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func recordRounds(ld *loopDetector, rounds int, gen func(i int) toolRecord) {
	for i := range rounds {
		ld.record([]toolRecord{gen(i)})
	}
}
