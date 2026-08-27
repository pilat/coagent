package telegram

import (
	"fmt"
	"os"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
)

const (
	syntheticHeader      = "The user attached a file:"
	imageAdvisory        = "\n\nUse the read tool on this path to view the image."
	maxImageAdvisorySize = 5 * 1024 * 1024 // mirrors read's cap: advice only where read can accept
)

// buildSyntheticMessage renders the fixed-EN template (D12). Exactly one
// conditional advisory sentence, iff the saved file sniffs into the canonical
// image set and is small enough for read to accept it; anything else gets
// metadata plus caption only.
func buildSyntheticMessage(name, sizeText, path, caption string) string {
	var sb strings.Builder

	sb.WriteString(syntheticHeader + "\n")
	fmt.Fprintf(&sb, "- name: %s\n", name)
	fmt.Fprintf(&sb, "- size: %s\n", sizeText)
	fmt.Fprintf(&sb, "- path: %s\n", path)

	if caption != "" {
		sb.WriteString("\n" + caption)
	}

	sb.WriteString(imageAdvisoryFor(path))

	return sb.String()
}

// imageAdvisoryFor decides the advice off the saved artifact itself — its
// sniffed magic bytes and real on-disk size beat any untrusted claim.
func imageAdvisoryFor(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxImageAdvisorySize {
		return ""
	}

	if !llmwire.IsSupportedImageMime(sniffImageMIME(path)) {
		return ""
	}

	return imageAdvisory
}

// sniffImageMIME maps the saved file's magic bytes onto the canonical wire MIME
// set (D13 parity with tool/builtin); "" means not-an-image to coagent.
func sniffImageMIME(path string) string {
	buf := make([]byte, 12)

	file, err := os.Open(path)
	if err != nil {
		return ""
	}

	defer func() { _ = file.Close() }()

	n, err := file.Read(buf)
	if err != nil || n < 3 {
		return ""
	}

	buf = buf[:n]

	switch {
	case buf[0] == 0x89 && n >= 4 && string(buf[1:4]) == "PNG":
		return llmwire.MimeImagePng
	case buf[0] == 0xFF && buf[1] == 0xD8 && buf[2] == 0xFF:
		return llmwire.MimeImageJpeg
	case n >= 4 && string(buf[:4]) == "GIF8":
		return llmwire.MimeImageGif
	case n >= 12 && string(buf[:4]) == "RIFF" && string(buf[8:12]) == "WEBP":
		return llmwire.MimeImageWebp
	default:
		return ""
	}
}

// scrubName flattens control characters out of an original filename so user
// bytes can never forge synthetic-template structure (`- path:` lines etc.).
func scrubName(name string) string {
	var sb strings.Builder

	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}

		sb.WriteRune(r)
	}

	cleaned := sb.String()
	if cleaned == "" {
		return "attachment"
	}

	return cleaned
}
