package llm

import (
	"os"
	"slices"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
)

// Shared capability gate + disk materialization for both driver families
// (anthropic.go, openai_http.go). Fail-closed: unknown modality info means no
// pixels ever reach the wire.
const visionModality = "image"

func supportsVision(inputModalities []string) bool {
	return slices.Contains(inputModalities, visionModality)
}

// classifyImage explains why an image slot must degrade to text; empty means
// the model accepts it, the MIME is canonical and the file exists.
func classifyImage(inputModalities []string, ref llmwire.ImageRef) string {
	if !supportsVision(inputModalities) {
		return llmwire.ImageOmitReasonNoVision
	}

	if !llmwire.IsSupportedImageMime(ref.Mime) {
		return llmwire.ImageOmitReasonUnsupported
	}

	if _, err := os.Stat(ref.Path); err != nil {
		return llmwire.ImageOmitReasonUnreadable
	}

	return ""
}

// resolveImage reads an eligible image's bytes off disk. A non-empty reason
// names why pixels cannot be sent — including failures striking between the
// eligibility check and the read itself, so slot wording never goes blank.
func resolveImage(inputModalities []string, ref llmwire.ImageRef, log *zap.Logger) ([]byte, string) {
	reason := classifyImage(inputModalities, ref)
	if reason != "" {
		log.Debug("image_degraded", zap.String("path", ref.Path), zap.String("reason", reason))

		return nil, reason
	}

	data, err := os.ReadFile(ref.Path)
	if err != nil {
		log.Debug("image_degraded", zap.String("path", ref.Path), zap.Error(err))

		return nil, llmwire.ImageOmitReasonUnreadable
	}

	return data, ""
}
