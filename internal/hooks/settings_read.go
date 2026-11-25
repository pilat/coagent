package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/safefile"
)

func readGlobalSettings(ctx context.Context, path string) settingsFile {
	data, err := os.ReadFile(path)
	if err != nil {
		logSettingsReadError(ctx, path, err)
		return settingsFile{Path: path}
	}

	return decodeSettings(ctx, path, data)
}

func readProjectSettings(ctx context.Context, path string, access *safefile.Access) settingsFile {
	opened, err := access.Open(path)
	if err != nil {
		logSettingsReadError(ctx, path, err)
		return settingsFile{Path: path}
	}
	defer func() { _ = opened.File.Close() }()

	data, err := io.ReadAll(opened.File)
	if err != nil {
		logSettingsReadError(ctx, path, err)
		return settingsFile{Path: path}
	}

	return decodeSettings(ctx, path, data)
}

func decodeSettings(ctx context.Context, path string, data []byte) settingsFile {
	file := settingsFile{Path: path}
	if err := json.Unmarshal(data, &file); err != nil {
		logger.Ctx(ctx).Warn("hooks_settings_invalid", zap.String("path", path), zap.Error(err))
		return settingsFile{Path: path}
	}

	return file
}

func logSettingsReadError(ctx context.Context, path string, err error) {
	if errors.Is(err, os.ErrNotExist) {
		return
	}

	logger.Ctx(ctx).Warn("hooks_settings_invalid", zap.String("path", path), zap.Error(err))
}
