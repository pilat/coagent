package managerdiscovery

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/loader"
)

func (s *Service) ListModels(context.Context) (*controllerapi.ConfigModelsResultData, error) {
	var models []controllerapi.ConfigModelInfo

	if unified := s.unifiedConfig(); unified != nil {
		for _, model := range unified.Models {
			info := controllerapi.ConfigModelInfo{
				ID: model.ID, Name: model.Name, DisplayName: model.DisplayName,
				EffortLevels: model.EffortLevels, DefaultEffort: model.DefaultEffort,
				CatalogStale: model.CatalogStale,
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

func (s *Service) ListSkills(
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

// SpawnFavorites returns configured paths in order, or a non-nil empty slice.
func (s *Service) SpawnFavorites(context.Context) ([]string, error) {
	var favorites []string
	if unified := s.unifiedConfig(); unified != nil {
		favorites = unified.SpawnFavorites
	}

	if favorites == nil {
		favorites = []string{}
	}

	return favorites, nil
}
