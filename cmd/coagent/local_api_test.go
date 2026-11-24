package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managercontrol"
)

type localAPIControllerFactory struct {
	controllerapi.ManagerControllerFactory
	managers, apps []string
}

func (f *localAPIControllerFactory) ForManager(id string) controllerapi.Controller {
	f.managers = append(f.managers, id)
	return f.ManagerControllerFactory.ForManager(id)
}

func (f *localAPIControllerFactory) ForApp(id string, projects []string) controllerapi.Controller {
	f.apps = append(f.apps, id)
	return f.ManagerControllerFactory.ForApp(id, projects)
}

func TestStartLocalAPI_BindsOnlyKnownRoles(t *testing.T) {
	for _, role := range []string{config.APIRoleOperator, config.APIRoleApp, "unknown"} {
		t.Run(role, func(t *testing.T) {
			home, err := os.MkdirTemp("/tmp", "api-roles")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(home)) })
			t.Setenv("HOME", home)
			factory := &localAPIControllerFactory{ManagerControllerFactory: managercontrol.New(nil, nil, nil, nil, nil)}
			core, logs := observer.New(zapcore.ErrorLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))
			cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{API: config.APIConfig{
				Clients: []config.APIClientEntry{{ID: "client", Role: role, TokenSHA256: strings.Repeat("a", 64)}},
			}}}
			a := &app{}
			t.Cleanup(func() {
				for _, stop := range a.stops {
					require.NoError(t, stop.fn(context.Background()))
				}
			})
			startLocalAPI(ctx, a, cfg, factory)
			switch role {
			case config.APIRoleOperator:
				assert.Equal(t, []string{"client"}, factory.managers)
				assert.Empty(t, factory.apps)
			case config.APIRoleApp:
				assert.Equal(t, []string{"client"}, factory.apps)
				assert.Empty(t, factory.managers)
			default:
				assert.Empty(t, factory.managers)
				assert.Empty(t, factory.apps)
				entries := logs.FilterMessage("local_api_client_refused").All()
				require.Len(t, entries, 1)
				assert.Equal(t, "client", entries[0].ContextMap()["client_id"])
				assert.Equal(t, role, entries[0].ContextMap()["role"])
			}
		})
	}
}

func TestStartLocalAPI_WithoutClientsBindsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, unified := range []*config.UnifiedConfig{nil, {}} {
		a := &app{}
		startLocalAPI(t.Context(), a, &config.Config{UnifiedConfig: unified}, nil)
		_, err := os.Stat(filepath.Join(home, coagenthome.DirName, coagenthome.APISocketFileName))
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Empty(t, a.stops)
	}
}

func TestStartLocalAPI_BindFailureLeavesTheDaemonRunning(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "api-bind")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.APISocketFileName)
	require.NoError(t, os.MkdirAll(socket, 0o700))
	// A nonempty directory cannot be removed as a stale socket.
	require.NoError(t, os.WriteFile(filepath.Join(socket, "occupied"), []byte("keep"), 0o600))
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		API: config.APIConfig{Clients: []config.APIClientEntry{{
			ID: "test", Role: config.APIRoleOperator, TokenSHA256: strings.Repeat("a", 64),
		}}},
	}}
	a := &app{}
	require.NotPanics(t, func() { startLocalAPI(t.Context(), a, cfg, nil) })
	assert.Empty(t, a.stops)
	content, err := os.ReadFile(filepath.Join(socket, "occupied"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(content))
}
