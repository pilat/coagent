package session

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/llmwire"
)

// The numbers come from the incident: three page scans of identical pixel
// dimensions cost 4,699 / 4,700 / 4,699 provider tokens regardless of a 1.24×
// file-size spread, which fixes the charge at ⌈1615/28⌉ × ⌈2193/28⌉ = 58 × 79.
func TestImageTokenEstimatePatchQuantum(t *testing.T) {
	ref := llmwire.ImageRef{
		Path: "/tmp/page.png", Mime: llmwire.MimeImagePng, Size: 3_200_000,
		Width: 1615, Height: 2193,
	}

	assert.Equal(t, 4582, imageTokenEstimate(ref))
}

func TestImageTokenEstimateFallbackWithoutDimensions(t *testing.T) {
	assert.Equal(t, 0, imageTokenEstimate(llmwire.ImageRef{Size: 0}), "Size=0 stays free")
	assert.Equal(t, 1, imageTokenEstimate(llmwire.ImageRef{Size: 5}), "small files charge Size/4")
	assert.Equal(t, 8192, imageTokenEstimate(llmwire.ImageRef{Size: 1 << 30}), "huge files cap at the ceiling")
}
