package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/safefile"
)

type mcpAttachmentSink struct {
	projectID int64
	sessionID int64
	access    safefile.Access
}

var _ mcp.AttachmentSink = (*mcpAttachmentSink)(nil)

func (s *mcpAttachmentSink) Store(part mcp.BinaryPart) (mcp.Attachment, error) {
	if s.projectID <= 0 {
		return mcp.Attachment{}, errors.New("project ID is unavailable")
	}

	root, err := coagenthome.ProcessProjectDir(s.projectID)
	if err != nil {
		return mcp.Attachment{}, fmt.Errorf("locate project directory: %w", err)
	}

	dir := filepath.Join(root, coagenthome.MCPAttachmentsDirName, strconv.FormatInt(s.sessionID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return mcp.Attachment{}, fmt.Errorf("create attachment directory: %w", err)
	}

	sniffed := sniffImageMIMEBytes(part.Data)
	ext := ".bin"

	switch sniffed {
	case llmwire.MimeImagePng:
		ext = ".png"
	case llmwire.MimeImageJpeg:
		ext = ".jpg"
	case llmwire.MimeImageGif:
		ext = ".gif"
	case llmwire.MimeImageWebp:
		ext = ".webp"
	}

	file, err := os.CreateTemp(dir, "mcp-*"+ext)
	if err != nil {
		return mcp.Attachment{}, fmt.Errorf("create attachment: %w", err)
	}

	path := file.Name()
	if _, err = file.Write(part.Data); err != nil {
		_ = file.Close()
		return mcp.Attachment{}, fmt.Errorf("write attachment: %w", err)
	}

	if err = file.Close(); err != nil {
		return mcp.Attachment{}, fmt.Errorf("close attachment: %w", err)
	}

	note := fmt.Sprintf("%s: %s (%s, %d bytes)", part.Kind, path, part.MIME, len(part.Data))
	if part.Kind != "image" {
		return mcp.Attachment{Note: note}, nil
	}

	return s.imageAttachment(part, path, note, sniffed)
}

func (s *mcpAttachmentSink) imageAttachment(part mcp.BinaryPart, path, note, sniffed string) (mcp.Attachment, error) {
	reason := ""

	switch {
	case sniffed == "":
		reason = "unsupported image bytes"
	case part.MIME != sniffed:
		reason = "declared MIME does not match image bytes"
	case len(part.Data) > maxImageBytes:
		reason = "image exceeds attachment limit"
	}

	if reason != "" {
		return mcp.Attachment{Note: note + ": " + reason}, nil
	}

	opened, err := s.access.Open(path)
	if err != nil {
		return mcp.Attachment{Note: note + ": read authorization failed"}, nil
	}

	defer func() { _ = opened.File.Close() }()

	width, height := imageDimensionsFile(opened.File)
	digest := sha256.Sum256(part.Data)
	ref := llmwire.ImageRef{
		Path: opened.Path.Canonical, ReadRoot: opened.Path.ReadRoot, ReadRootID: opened.Path.ReadRootID,
		Mime: sniffed, Size: int64(len(part.Data)), Width: width, Height: height,
		Digest: hex.EncodeToString(digest[:]),
	}

	return mcp.Attachment{
		Note:  fmt.Sprintf("image attached (%s, %d bytes, %d×%d)", sniffed, len(part.Data), width, height),
		Image: &ref,
	}, nil
}
