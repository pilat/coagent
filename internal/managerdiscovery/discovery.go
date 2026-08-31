package managerdiscovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/loader"
)

func (s *service) ListModels(context.Context) (*controllerapi.ConfigModelsResultData, error) {
	var models []controllerapi.ConfigModelInfo

	if unified := s.unifiedConfig(); unified != nil {
		for _, model := range unified.Models {
			info := controllerapi.ConfigModelInfo{
				ID: model.ID, Name: model.Name, DisplayName: model.DisplayName,
				EffortLevels: model.EffortLevels, DefaultEffort: model.DefaultEffort,
			}
			if model.Pricing != nil {
				info.InputPrice = model.Pricing.InputPrice
				info.OutputPrice = model.Pricing.OutputPrice
			}

			models = append(models, info)
		}
	}

	defaultID := ""
	if len(models) > 0 {
		defaultID = models[0].ID
	}

	return &controllerapi.ConfigModelsResultData{Models: models, DefaultID: defaultID}, nil
}

func (s *service) ListSkills(
	ctx context.Context,
	managerID string,
	data controllerapi.ConfigSkillsData,
) (*controllerapi.ConfigSkillsResultData, error) {
	record, err := s.backend.GetSession(ctx, data.SessionID)
	if err != nil || record == nil {
		return nil, fmt.Errorf("session %d not found", data.SessionID)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if managerID == "" || owner != managerID {
		return nil, fmt.Errorf("session %d belongs to another manager", data.SessionID)
	}

	workDir, err := s.backend.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil {
		workDir = "."
	}

	reader := loader.New(s.cache)
	if unified := s.unifiedConfig(); unified != nil && len(unified.Marketplaces) > 0 {
		reader.ProcessMarketplaces(ctx, unified.Marketplaces, nil)
	}

	_ = reader.LoadSkills(workDir)
	list := reader.ListUserInvocableSkills()
	skills := make([]controllerapi.ConfigSkillInfo, len(list))

	for i, skill := range list {
		skills[i] = controllerapi.ConfigSkillInfo{Name: skill.Name, Description: skill.Description}
	}

	return &controllerapi.ConfigSkillsResultData{Skills: skills}, nil
}

func (s *service) ListDir(
	_ context.Context,
	data controllerapi.FsListDirData,
) (*controllerapi.FsListDirResultData, error) {
	var favorites []string
	if unified := s.unifiedConfig(); unified != nil {
		favorites = unified.SpawnFavorites
	}

	if favorites == nil {
		favorites = []string{}
	}

	home, homeErr := coagenthome.UserHome()

	path := data.Path
	if path == "" {
		if homeErr != nil {
			return nil, fmt.Errorf("home dir: %w", homeErr)
		}

		path = home
	}

	dirs, err := readSubdirs(path)
	if err != nil {
		return nil, err
	}

	parent := ""
	if path != "/" {
		parent = filepath.Dir(path)
	}

	return &controllerapi.FsListDirResultData{
		Dirs: dirs, Favorites: favorites, Home: home, Path: path, Parent: parent,
	}, nil
}

func readSubdirs(path string) ([]controllerapi.FsDirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("readdir: %w", err)
	}

	type dirWithMtime struct {
		name  string
		mtime time.Time
	}

	var filtered []dirWithMtime

	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == "" || name[0] == '.' {
			continue
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}

		filtered = append(filtered, dirWithMtime{name: name, mtime: info.ModTime()})
	}

	sort.Slice(filtered, func(i, j int) bool { return filtered[i].mtime.After(filtered[j].mtime) })

	dirs := make([]controllerapi.FsDirEntry, 0, len(filtered))
	for _, dir := range filtered {
		dirs = append(dirs, controllerapi.FsDirEntry{Name: dir.name, Path: filepath.Join(path, dir.name)})
	}

	return dirs, nil
}
