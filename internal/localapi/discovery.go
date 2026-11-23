package localapi

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/controllerapi"
)

type discoveryService struct{}

func (discoveryService) ListModels(
	ctx context.Context,
	_ *coagentv1.ListModelsRequest,
) (*coagentv1.ListModelsResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	result, err := author.control.ListModels(ctx)
	if err != nil {
		return nil, refusal(ctx, err)
	}

	response := &coagentv1.ListModelsResponse{}
	for _, entry := range result.Models {
		response.Models = append(response.Models, model(entry))
	}

	return response, nil
}

func (discoveryService) ListSkills(
	ctx context.Context,
	req *coagentv1.ListSkillsRequest,
) (*coagentv1.ListSkillsResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	result, err := author.control.ListSkills(ctx, controllerapi.ConfigSkillsData{SessionID: req.GetSessionId()})
	if err != nil {
		return nil, refusal(ctx, err)
	}

	response := &coagentv1.ListSkillsResponse{}
	for _, entry := range result.Skills {
		response.Skills = append(response.Skills, skill(entry))
	}

	return response, nil
}

func (discoveryService) ListProjects(
	ctx context.Context,
	_ *coagentv1.ListProjectsRequest,
) (*coagentv1.ListProjectsResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	result, err := author.control.ListRecentProjects(ctx)
	if err != nil {
		return nil, refusal(ctx, err)
	}

	response := &coagentv1.ListProjectsResponse{}
	for _, entry := range result.Projects {
		response.Projects = append(response.Projects, project(entry))
	}

	return response, nil
}

func (discoveryService) ListSpawnFavorites(
	ctx context.Context,
	_ *coagentv1.ListSpawnFavoritesRequest,
) (*coagentv1.ListSpawnFavoritesResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	paths, err := author.control.SpawnFavorites(ctx)
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.ListSpawnFavoritesResponse{Paths: paths}, nil
}

func (discoveryService) CreateProject(
	ctx context.Context,
	req *coagentv1.CreateProjectRequest,
) (*coagentv1.CreateProjectResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	result, err := author.control.CreateProject(ctx, controllerapi.ProjectCreateData{Name: req.GetName()})
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.CreateProjectResponse{Path: result.Path}, nil
}

func model(entry controllerapi.ConfigModelInfo) *coagentv1.Model {
	return &coagentv1.Model{
		Id: entry.ID, Name: entry.Name, DisplayName: entry.DisplayName,
		InputPrice: entry.InputPrice, OutputPrice: entry.OutputPrice,
		EffortLevels: entry.EffortLevels, DefaultEffort: entry.DefaultEffort, CatalogStale: entry.CatalogStale,
	}
}

func skill(entry controllerapi.ConfigSkillInfo) *coagentv1.Skill {
	return &coagentv1.Skill{Name: entry.Name, Description: entry.Description}
}

func project(entry controllerapi.RecentProjectInfo) *coagentv1.Project {
	result := &coagentv1.Project{Name: entry.Name}
	if entry.LastActivity != nil {
		result.LastActivity = timestamppb.New(*entry.LastActivity)
	}

	return result
}
