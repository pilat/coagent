package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapOrderTrials is the number of repetitions used to expose logic that depends
// on Go's randomized map iteration order. A single pass would catch it only by
// luck.
const mapOrderTrials = 50

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
