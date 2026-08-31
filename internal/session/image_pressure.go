package session

import (
	"github.com/pilat/coagent/internal/llmwire"
)

// Image byte/count pressure marks (D4/D5). Bytes are base64 wire sizes: the
// observed incident wall sits at 35.8/38.8 MB, one observation of a
// load-dependent time wall, so the trigger keeps a ×3 margin. Counts use
// Bedrock's published per-request limit. Low-water is half of high-water on
// both axes, giving the boundary hysteresis of roughly one image batch.
const (
	imageBytesHighWater = 12 << 20 // 12 MB base64 — compaction trigger
	imageBytesLowWater  = 6 << 20  // 6 MB base64 — tail ceiling
	imageCountHighWater = 20       // compaction trigger
	imageCountLowWater  = 10       // tail ceiling
	// imagePatchQuantumPx is the measured token quantum per pixel patch,
	// exact for the incident model (D4/T5).
	imagePatchQuantumPx = 28
)

// imageBase64Bytes is the wire size of one stored attachment, derived from its
// recorded size. No disk access keeps the projection deterministic, and a file
// that vanished mid-session still counts: it cannot free budget or make the
// boundary retreat.
func imageBase64Bytes(size int64) int64 {
	return (size + 2) / 3 * 4
}

// imagePressure sums the base64 wire bytes and the count of every attachment
// the transcript would carry, including refs a driver would degrade —
// conservatism is the point. Both the compaction trigger and the tail ceiling
// read the same projection.
func imagePressure(messages []llmwire.Message) (int64, int) {
	var totalBytes int64

	count := 0

	for _, msg := range messages {
		for _, ref := range msg.Images {
			totalBytes += imageBase64Bytes(ref.Size)
			count++
		}
	}

	return totalBytes, count
}
