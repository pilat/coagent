package localapi

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
)

var (
	_ controllerapi.ManagerControllerFactory = apiStubFactory{}
	_ controllerapi.Controller               = (*discoveryStub)(nil)
	_ controllerapi.ChatFocusController      = (*discoveryStub)(nil)
	_ controllerapi.LineageController        = (*discoveryStub)(nil)
)

type apiStubFactory struct {
	control controllerapi.Controller
}

type discoveryStub struct {
	controllerapi.Controller
	controllerapi.ChatFocusController
	models   []controllerapi.ConfigModelInfo
	skills   []controllerapi.ConfigSkillInfo
	projects []controllerapi.RecentProjectInfo
	paths    []string
	created  string
	skillID  int64
	name     string
}

func (f apiStubFactory) ForManager(string) controllerapi.Controller {
	return f.control
}

func (f apiStubFactory) ForApp(string, []string) controllerapi.Controller {
	return f.control
}

func (s *discoveryStub) Successor(context.Context, int64) (int64, error) {
	return 0, nil
}

func (s *discoveryStub) ListModels(context.Context) (*controllerapi.ConfigModelsResultData, error) {
	return &controllerapi.ConfigModelsResultData{Models: s.models}, nil
}

func (s *discoveryStub) ListSkills(
	_ context.Context,
	data controllerapi.ConfigSkillsData,
) (*controllerapi.ConfigSkillsResultData, error) {
	s.skillID = data.SessionID
	return &controllerapi.ConfigSkillsResultData{Skills: s.skills}, nil
}

func (s *discoveryStub) ListRecentProjects(context.Context) (*controllerapi.ProjectListResultData, error) {
	return &controllerapi.ProjectListResultData{Projects: s.projects}, nil
}

func (s *discoveryStub) SpawnFavorites(context.Context) ([]string, error) {
	return s.paths, nil
}

func (s *discoveryStub) CreateProject(
	_ context.Context,
	data controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
	s.name = data.Name
	return &controllerapi.ProjectCreateResultData{Path: s.created}, nil
}

func TestDiscoveryService_MapsEveryRenderedField(t *testing.T) {
	t.Parallel()
	activity := time.Date(2026, time.November, 23, 12, 34, 56, 789, time.UTC)
	stub := &discoveryStub{
		models: []controllerapi.ConfigModelInfo{
			{
				ID: "first", Name: "First", DisplayName: "First display", InputPrice: 1.25, OutputPrice: 5.5,
				EffortLevels: []string{"low", "high"}, DefaultEffort: "high", CatalogStale: true,
			},
			{
				ID: "second", Name: "Second", DisplayName: "Second display", InputPrice: 2.75, OutputPrice: 9.5,
				EffortLevels: []string{"minimal", "medium"}, DefaultEffort: "medium", CatalogStale: false,
			},
		},
		skills: []controllerapi.ConfigSkillInfo{
			{Name: "build", Description: "Build a project"}, {Name: "inspect"},
		},
		projects: []controllerapi.RecentProjectInfo{
			{Name: "active", LastActivity: &activity}, {Name: "empty"},
		},
		paths: []string{"/projects/first", "/projects/second"}, created: "/projects/new",
	}
	transport, err := NewInProcess(t.Context(), apiStubFactory{control: stub}).Transport("alpha")
	require.NoError(t, err)
	client := coagentv1connect.NewDiscoveryServiceClient(connect.NewClient(transport))
	models, err := client.ListModels(t.Context(), &coagentv1.ListModelsRequest{})
	require.NoError(t, err)
	require.Len(t, models.GetModels(), len(stub.models))
	for i, expected := range stub.models {
		actual := models.GetModels()[i]
		assert.Equal(t, expected.ID, actual.GetId())
		assert.Equal(t, expected.Name, actual.GetName())
		assert.Equal(t, expected.DisplayName, actual.GetDisplayName())
		assert.InDelta(t, expected.InputPrice, actual.GetInputPrice(), 0.000001)
		assert.InDelta(t, expected.OutputPrice, actual.GetOutputPrice(), 0.000001)
		assert.Equal(t, expected.EffortLevels, actual.GetEffortLevels())
		assert.Equal(t, expected.DefaultEffort, actual.GetDefaultEffort())
		assert.Equal(t, expected.CatalogStale, actual.GetCatalogStale())
	}
	skills, err := client.ListSkills(t.Context(), &coagentv1.ListSkillsRequest{SessionId: 42})
	require.NoError(t, err)
	assert.Equal(t, int64(42), stub.skillID)
	require.Len(t, skills.GetSkills(), len(stub.skills))
	for i, expected := range stub.skills {
		assert.Equal(t, expected.Name, skills.GetSkills()[i].GetName())
		assert.Equal(t, expected.Description, skills.GetSkills()[i].GetDescription())
	}
	projects, err := client.ListProjects(t.Context(), &coagentv1.ListProjectsRequest{})
	require.NoError(t, err)
	require.Len(t, projects.GetProjects(), 2)
	assert.Equal(t, "active", projects.GetProjects()[0].GetName())
	require.NotNil(t, projects.GetProjects()[0].GetLastActivity())
	assert.Equal(t, activity, projects.GetProjects()[0].GetLastActivity().AsTime())
	assert.Equal(t, "empty", projects.GetProjects()[1].GetName())
	assert.Nil(t, projects.GetProjects()[1].GetLastActivity())
	favorites, err := client.ListSpawnFavorites(t.Context(), &coagentv1.ListSpawnFavoritesRequest{})
	require.NoError(t, err)
	assert.Equal(t, stub.paths, favorites.GetPaths())
	created, err := client.CreateProject(t.Context(), &coagentv1.CreateProjectRequest{Name: "new"})
	require.NoError(t, err)
	assert.Equal(t, "new", stub.name)
	assert.Equal(t, stub.created, created.GetPath())
}
