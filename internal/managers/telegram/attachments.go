package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/humanize"
	"github.com/pilat/coagent/internal/logger"
)

// handleAttachment ingests one uploaded file: download → /tmp under a random
// name → confirmation echo → synthetic metadata message into the session.
// No attachment payload crosses any manager boundary — controllerapi stays
// strings-only; seeing pixels is read's job.
func (m *Manager) handleAttachment(
	ctx context.Context,
	msg *telegramMessage,
	att *tgAttachment,
	threadID, sessionID int64,
	hasSession bool,
) {
	if !hasSession {
		return // no-session topics drop silently (voice precedent)
	}

	log := logger.Ctx(ctx).Named("telegram.attachments")

	_ = m.sendTyping(ctx, threadID)

	savedPath, err := m.saveAttachment(ctx, att)
	if err != nil {
		log.Error("attachment_save_failed", zap.String("name", att.name), zap.Error(err))
		_, _ = m.sendMessage(ctx, "❌ Failed to save "+att.displayName()+": "+err.Error(), nil, threadID)

		return
	}

	sizeText := att.sizeText()
	name := att.nameText(savedPath)

	// Doubles as failure visibility: when nothing arrives here but no error did,
	// the download produced an empty artifact worth investigating in-chat.
	_, _ = m.sendMessage(
		ctx,
		fmt.Sprintf("📎 %s (%s) → %s", name, sizeText, savedPath),
		nil,
		threadID,
	)

	synthetic := buildSyntheticMessage(name, sizeText, savedPath, msg.Caption)
	m.handleSessionMessage(ctx, sessionID, synthetic, threadID)
}

// tgAttachment normalizes the four upload shapes behind one accessor set.
type tgAttachment struct {
	fileID   string
	name     string // original name; empty for photos
	mimeType string
	fileSize int64  // 0 when Telegram omitted it → rendered as "unknown"
	kind     string // document | photo | video | audio
}

func (a *tgAttachment) displayName() string {
	if a.name != "" {
		return scrubName(a.name)
	}

	return "the attachment"
}

// nameText renders the synthetic message's `name:` line: Telegram's own name,
// then per-kind defaults. Original names never shape path construction.
func (a *tgAttachment) nameText(savedPath string) string {
	switch {
	case a.name != "":
		return scrubName(filepath.Base(a.name))
	case a.kind == "photo":
		return "photo.jpg"
	case a.kind == "video":
		return "video.mp4"
	case a.kind == "audio" && a.mimeType != "":
		// mime-derived fallback is audio-only per D12: "audio/ogg" → "audio.ogg"
		sub := a.mimeType
		if i := strings.IndexByte(sub, '/'); i >= 0 {
			sub = sub[i+1:]
		}

		if i := strings.IndexByte(sub, ';'); i >= 0 {
			sub = sub[:i]
		}

		return "audio" + sanitizeExtension(sub)
	default:
		return filepath.Base(savedPath)
	}
}

func (a *tgAttachment) sizeText() string {
	if a.fileSize <= 0 {
		return "unknown"
	}

	return humanize.FormatSize(a.fileSize)
}

func attachmentOf(msg *telegramMessage) *tgAttachment {
	switch {
	case msg.Document != nil:
		d := msg.Document

		return &tgAttachment{
			fileID:   d.FileID,
			name:     d.FileName,
			mimeType: d.MimeType,
			fileSize: d.FileSize,
			kind:     "document",
		}
	case len(msg.Photo) > 0:
		// Albums arrive as N independent messages; pick the largest resolution.
		largest := msg.Photo[0]
		for _, p := range msg.Photo[1:] {
			if p.Width*p.Height > largest.Width*largest.Height {
				largest = p
			}
		}

		return &tgAttachment{fileID: largest.FileID, fileSize: largest.FileSize, kind: "photo"}
	case msg.Video != nil:
		v := msg.Video

		return &tgAttachment{
			fileID:   v.FileID,
			name:     v.FileName,
			mimeType: v.MimeType,
			fileSize: v.FileSize,
			kind:     "video",
		}
	case msg.Audio != nil:
		a := msg.Audio

		return &tgAttachment{
			fileID:   a.FileID,
			name:     a.FileName,
			mimeType: a.MimeType,
			fileSize: a.FileSize,
			kind:     "audio",
		}
	default:
		return nil
	}
}

func hasAttachment(msg *telegramMessage) bool {
	return attachmentOf(msg) != nil
}

// saveAttachment resolves the bot-API file and streams it straight into
// os.CreateTemp("", "coagent-*"+ext): O_EXCL naming keeps collisions away.
func (m *Manager) saveAttachment(ctx context.Context, att *tgAttachment) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, attachmentDownloadTimeout)
	defer cancel()

	tgFilePath, err := m.getTelegramFilePath(dctx, att.fileID, m.downloadClient)
	if err != nil {
		return "", attachmentResolveError(err)
	}

	ext := sanitizeExtension(firstExtension(att.name, tgFilePath))

	tmp, err := os.CreateTemp("", "coagent-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	path := tmp.Name()

	written, writeErr := m.downloadToFile(dctx, tgFilePath, tmp)

	closeErr := tmp.Close()

	if writeErr != nil || closeErr != nil || written == 0 {
		_ = os.Remove(path)

		if written == 0 && writeErr == nil && closeErr == nil {
			return "", errors.New("downloaded attachment contained no data")
		}

		return "", fmt.Errorf("write attachment: %w", pickError(writeErr, closeErr))
	}

	return path, nil
}

func attachmentResolveError(err error) error {
	var apiErr *tgAPIError

	tooBig := errors.As(err, &apiErr) && apiErr.ErrorCode == 400 &&
		strings.Contains(strings.ToLower(apiErr.Description), "file is too big")
	if tooBig {
		return errors.New(
			"telegram Bot API rejected this file as too big; files over 20 MB require " +
				"api_url pointing to a Bot API server running in local mode",
		)
	}

	return fmt.Errorf("resolve file id: %w", err)
}

func pickError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// sanitizeExtension filters an extension candidate to [A-Za-z0-9], ≤10 chars;
// empty when absent. This is the only way original names touch paths.
func sanitizeExtension(rawExt string) string {
	var cleaned strings.Builder

	for _, r := range rawExt {
		isASCIIAlnum := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if !isASCIIAlnum {
			continue
		}

		cleaned.WriteRune(r)
	}

	const maxExtRunes = 10

	ext := cleaned.String()
	if len(ext) > maxExtRunes {
		ext = ext[:maxExtRunes]
	}

	if ext == "" {
		return ""
	}

	return "." + ext
}

// firstExtension takes the longest filename/tg-path extension available.
func firstExtension(names ...string) string {
	best := ""

	for _, name := range names {
		ext := filepath.Ext(filepath.Base(strings.TrimSpace(name)))
		if trimmed := strings.TrimPrefix(ext, "."); len(trimmed) > len(best) {
			best = trimmed
		}
	}

	return best
}
