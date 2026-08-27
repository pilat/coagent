package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
)

// maxImageBytes equals the strictest per-image provider limit across coagent's
// driver families (Vertex/Bedrock). A slightly-over-cap image must fail here,
// at read time, rather than enter history and poison every replayed turn.
const maxImageBytes = 5 * 1024 * 1024

var _ tool.Tool = (*readTool)(nil)

// sniffImageMIME returns the canonical wire MIME for files whose magic bytes
// match, or "" otherwise. Sniffs bytes, never extensions.
func sniffImageMIME(path string) string {
	buf := make([]byte, 12)

	file, err := os.Open(path)
	if err != nil {
		return ""
	}

	defer func() { _ = file.Close() }()

	n, err := file.Read(buf)
	if err != nil || n == 0 {
		return ""
	}

	buf = buf[:n]

	switch {
	case bytes.HasPrefix(buf, []byte{0x89, 'P', 'N', 'G'}):
		return llmwire.MimeImagePng
	case bytes.HasPrefix(buf, []byte{0xFF, 0xD8, 0xFF}):
		return llmwire.MimeImageJpeg
	case bytes.HasPrefix(buf, []byte("GIF8")):
		return llmwire.MimeImageGif
	case n >= 12 && bytes.HasPrefix(buf, []byte("RIFF")) && bytes.Equal(buf[8:12], []byte("WEBP")):
		return llmwire.MimeImageWebp
	default:
		return ""
	}
}

// readImage renders a supported image as a Result carrying the image ref. The
// branch ignores offset/limit entirely: pixels have no line pagination.
func (t *readTool) readImage(ctx context.Context, filePath, mime string) (*tool.Result, error) {
	log := logger.Ctx(ctx).Named("tool.read")

	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("stat image file: %w", err)
	}

	if info.Size() > maxImageBytes {
		log.Warn("image_too_large", zap.String("filePath", filePath), zap.Int64("size", info.Size()))

		return nil, fmt.Errorf(
			"cannot read image %s: %d bytes exceeds the %d byte limit; resize or convert it first",
			filePath, info.Size(), maxImageBytes,
		)
	}

	title := relativeTitle(t.workDir, filePath)

	log.Debug("image_read", zap.String("filePath", filePath), zap.String("mime", mime))

	return &tool.Result{
		Title: title,
		Output: fmt.Sprintf(
			"<image>\npath: %s\ntype: %s\nsize: %d bytes\n</image>\n(Image attached to this result for viewing)",
			title, mime, info.Size(),
		),
		Metadata: map[string]any{"mime": mime},
		Images:   []llmwire.ImageRef{{Path: filePath, Mime: mime, Size: info.Size()}},
	}, nil
}
