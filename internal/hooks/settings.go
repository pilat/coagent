package hooks

import (
	"context"
	"fmt"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/safefile"
)

func loadSettings(ctx context.Context, workDir string) (map[string][]handler, error) {
	globalPath, err := coagenthome.Join(coagenthome.SettingsFileName)
	if err != nil {
		return nil, fmt.Errorf("resolve hook settings: %w", err)
	}

	global := readGlobalSettings(ctx, globalPath)
	if global.DisableAllHooks != nil && *global.DisableAllHooks {
		logger.Ctx(ctx).Info("hooks_disabled", zap.String("path", global.Path))
		return map[string][]handler{}, nil
	}

	home, err := coagenthome.UserHome()
	if err != nil {
		return nil, fmt.Errorf("resolve hook project home: %w", err)
	}

	canonicalHome, resolveErr := filepath.EvalSymlinks(home)
	if resolveErr != nil {
		canonicalHome = filepath.Clean(home)
	}

	if workDir == canonicalHome {
		logger.Ctx(ctx).Debug("hooks_project_settings_skipped", zap.String("path", workDir))
		return mergeSettings(ctx, []settingsFile{global}), nil
	}

	project, err := loadProjectSettings(ctx, workDir)
	if err != nil {
		return nil, err
	}

	if projectHooksDisabled(ctx, project) {
		return mergeSettings(ctx, []settingsFile{global}), nil
	}

	files := append([]settingsFile{global}, project...)

	return mergeSettings(ctx, files), nil
}

func loadProjectSettings(ctx context.Context, workDir string) ([]settingsFile, error) {
	access, err := safefile.New(safefile.ProjectPolicy(workDir), workDir)
	if err != nil {
		return nil, fmt.Errorf("open hook settings project: %w", err)
	}
	defer func() { _ = access.Close() }()

	paths := []string{
		filepath.Join(workDir, config.ProjectConfigDir, "settings.json"),
		filepath.Join(workDir, config.ProjectConfigDir, "settings.local.json"),
	}

	files := make([]settingsFile, 0, len(paths))
	for _, path := range paths {
		files = append(files, readProjectSettings(ctx, path, access))
	}

	return files, nil
}

func projectHooksDisabled(ctx context.Context, files []settingsFile) bool {
	disabledBy := ""

	for _, file := range files {
		if file.DisableAllHooks == nil {
			continue
		}

		if *file.DisableAllHooks {
			disabledBy = file.Path
		} else {
			disabledBy = ""
		}
	}

	if disabledBy == "" {
		return false
	}

	logger.Ctx(ctx).Info("hooks_disabled", zap.String("path", disabledBy))

	return true
}
