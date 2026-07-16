package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
)

const (
	loopDetectorWindowSize       = 20
	loopDetectorMinFill          = 10
	loopDetectorConsecutiveWarn  = 3
	loopDetectorJaccardThreshold = 0.7
	loopDetectorDiversityWarn    = 0.35
	loopDetectorMaxBlocks        = 3
	loopDetectorFailWarn         = 3 // same tool + same error N times in a row -> warn
	loopDetectorFailBlock        = 5 // ...and M times -> block
)

const (
	actionNone          loopAction = iota
	actionWarn                     // prepend warning to last tool result
	actionBlock                    // don't execute tools, return block message
	actionForceTextOnly            // pass empty tools slice to LLM
	actionWarnFailure              // prepend repeated-failure warning to last tool result
)

// loopAction represents the detector's verdict after checking the window.
type loopAction int

type toolRecord struct {
	name       string
	argsHash   uint64
	resultHash uint64
	failed     bool // tool call returned an error (not a normal result)
}

// argKey is the fingerprint key for Jaccard comparison — excludes resultHash
// so that flaky-output loops (same command, varying results) can still escalate.
type argKey struct {
	name     string
	argsHash uint64
}

type loopDetector struct {
	window            []toolRecord
	warnFingerprint   map[argKey]struct{}
	warnActive        bool
	blocked           bool
	consecutiveBlocks int
	forceTextOnly     bool
}

func newLoopDetector() *loopDetector {
	return &loopDetector{
		window: make([]toolRecord, 0, loopDetectorWindowSize),
	}
}

// record adds records for a round. Deduplicates by full tuple within the round.
func (ld *loopDetector) record(records []toolRecord) {
	seen := make(map[toolRecord]struct{}, len(records))
	for _, r := range records {
		if _, ok := seen[r]; ok {
			continue
		}

		seen[r] = struct{}{}

		ld.window = append(ld.window, r)
	}

	// Trim to window capacity
	if len(ld.window) > loopDetectorWindowSize {
		excess := len(ld.window) - loopDetectorWindowSize
		ld.window = ld.window[excess:]
	}
}

// check returns the detector's verdict for the current window state.
func (ld *loopDetector) check() loopAction {
	// Step 1: Already in force-text-only mode
	if ld.forceTextOnly {
		return actionForceTextOnly
	}

	// Step 2: Already blocked
	if ld.blocked {
		ld.consecutiveBlocks++
		if ld.consecutiveBlocks >= loopDetectorMaxBlocks {
			ld.forceTextOnly = true
			return actionForceTextOnly
		}

		return actionBlock
	}

	// Step 3: Repeated identical failure — same tool + same error, regardless of
	// args. Catches loops where the model rerolls arguments but keeps hitting the
	// same error (arg diversity stays high, so the diversity path never fires).
	switch streak := ld.consecutiveFailureStreak(); {
	case streak >= loopDetectorFailBlock:
		ld.blocked = true
		return actionBlock
	case streak >= loopDetectorFailWarn:
		return actionWarnFailure
	}

	// Step 4: Fast-path — last N consecutive identical records
	if ld.hasConsecutiveIdentical() {
		return ld.warnOrEscalate()
	}

	// Step 5: Minimum fill check
	if len(ld.window) < loopDetectorMinFill {
		return actionNone
	}

	// Steps 6-8: Compute dual diversity
	argsDiversity := ld.uniqueArgsFraction()
	resultDiversity := ld.uniqueResultsFraction()
	diversity := argsDiversity

	if resultDiversity < diversity {
		diversity = resultDiversity
	}

	// Step 9: Healthy diversity — reset all state
	if diversity >= loopDetectorDiversityWarn {
		ld.warnActive = false
		ld.warnFingerprint = nil
		ld.blocked = false
		ld.consecutiveBlocks = 0

		return actionNone
	}

	// Steps 10-11: Low diversity — warn or escalate
	return ld.warnOrEscalate()
}

// consecutiveFailureStreak counts the trailing records that all failed and share
// the last record's (name, resultHash) — i.e. the same tool returning the same
// error in an unbroken run. Any success or differing failure breaks the streak.
func (ld *loopDetector) consecutiveFailureStreak() int {
	n := len(ld.window)
	if n == 0 {
		return 0
	}

	last := ld.window[n-1]
	if !last.failed {
		return 0
	}

	streak := 0

	for i := n - 1; i >= 0; i-- {
		r := ld.window[i]
		if !r.failed || r.name != last.name || r.resultHash != last.resultHash {
			break
		}

		streak++
	}

	return streak
}

// warnOrEscalate handles the warn -> block escalation.
// Fingerprinting uses (name, argsHash) only — not resultHash — so that
// flaky-output loops (same command, varying results) can still escalate.
func (ld *loopDetector) warnOrEscalate() loopAction {
	if ld.warnActive {
		currentSet := windowAsArgSet(ld.window)
		if jaccardSimilarityArgs(currentSet, ld.warnFingerprint) >= loopDetectorJaccardThreshold {
			ld.blocked = true
			return actionBlock
		}
	}

	// Issue a new warning
	ld.warnActive = true
	ld.warnFingerprint = windowAsArgSet(ld.window)

	return actionWarn
}

// hasConsecutiveIdentical checks if the last loopDetectorConsecutiveWarn records are identical.
func (ld *loopDetector) hasConsecutiveIdentical() bool {
	n := loopDetectorConsecutiveWarn
	if len(ld.window) < n {
		return false
	}

	last := ld.window[len(ld.window)-1]
	for i := len(ld.window) - n; i < len(ld.window)-1; i++ {
		if ld.window[i] != last {
			return false
		}
	}

	return true
}

// uniqueArgsFraction returns unique (name, argsHash) / len(window).
func (ld *loopDetector) uniqueArgsFraction() float64 {
	type key struct {
		name     string
		argsHash uint64
	}

	seen := make(map[key]struct{}, len(ld.window))
	for _, r := range ld.window {
		seen[key{r.name, r.argsHash}] = struct{}{}
	}

	return float64(len(seen)) / float64(len(ld.window))
}

// uniqueResultsFraction returns unique (name, resultHash) / len(window).
func (ld *loopDetector) uniqueResultsFraction() float64 {
	type key struct {
		name       string
		resultHash uint64
	}

	seen := make(map[key]struct{}, len(ld.window))
	for _, r := range ld.window {
		seen[key{r.name, r.resultHash}] = struct{}{}
	}

	return float64(len(seen)) / float64(len(ld.window))
}

// resetWindow clears all state. Called only from steering message drain.
func (ld *loopDetector) resetWindow() {
	ld.window = ld.window[:0]
	ld.warnActive = false
	ld.warnFingerprint = nil
	ld.blocked = false
	ld.consecutiveBlocks = 0
	ld.forceTextOnly = false
}

// clearForceTextOnly resets force-text-only mode after the LLM produces a text response.
// Clears the window too so the escalation restarts from scratch — otherwise the stale
// records (e.g. a failure streak) would immediately re-trigger and block the retry.
func (ld *loopDetector) clearForceTextOnly() {
	ld.forceTextOnly = false
	ld.blocked = false
	ld.consecutiveBlocks = 0
	ld.warnActive = false
	ld.warnFingerprint = nil
	ld.window = ld.window[:0]
}

// fingerprintResult hashes a tool result using FNV-64.
// Uses length + head + tail for stable fingerprinting of large outputs.
func fingerprintResult(result string) uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%s:%s", len(result), headBytes(result, 256), tailBytes(result, 256))

	return h.Sum64()
}

// fingerprintArgs hashes compacted JSON args using FNV-64.
func fingerprintArgs(args []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(compactJSON(args)))

	return h.Sum64()
}

func headBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}

	return s[:n]
}

func tailBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}

	return s[len(s)-n:]
}

// compactJSON normalizes JSON by removing insignificant whitespace.
// Falls back to raw string if input is not valid JSON.
func compactJSON(data []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return string(data)
	}

	return buf.String()
}

// windowAsArgSet converts a slice of toolRecords to a set of argKeys (name, argsHash).
func windowAsArgSet(window []toolRecord) map[argKey]struct{} {
	s := make(map[argKey]struct{}, len(window))
	for _, r := range window {
		s[argKey{r.name, r.argsHash}] = struct{}{}
	}

	return s
}

// jaccardSimilarityArgs computes |intersection| / |union| of two argKey sets.
func jaccardSimilarityArgs(a, b map[argKey]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0.0
	}

	intersection := 0

	for k := range a {
		if _, ok := b[k]; ok {
			intersection++
		}
	}

	union := len(a)

	for k := range b {
		if _, ok := a[k]; ok {
			continue
		}

		union++
	}

	return float64(intersection) / float64(union)
}
