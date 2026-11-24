package main

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/localapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managers"
	"github.com/pilat/coagent/internal/version"
)

var _ localapi.ManagerRuntime = (*managers.Runtime)(nil)

func startLocalAPI(
	ctx context.Context,
	a *app,
	cfg *config.Config,
	controllers controllerapi.ManagerControllerFactory,
	managerRuntime localapi.ManagerRuntime,
) (*localapi.Server, error) {
	path, err := coagenthome.Join(coagenthome.SocketFileName)
	if err != nil {
		return nil, err
	}

	listener, err := localapi.ListenUnix(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}

	srv := localapi.New(ctx, listener, localapi.StatusSource{
		Config: cfg, ConfigPath: config.DefaultUnifiedConfigFile, Version: version.Version, Managers: managerRuntime,
	})
	attachConfiguredAPI(ctx, srv, cfg, controllers)
	log := logger.Ctx(ctx).Named("main.localapi")

	go func() {
		if err := srv.ServeStatus(); err != nil {
			log.Error("local_api_serve_failed", zap.String("socket", coagenthome.SocketFileName), zap.Error(err))
		}
	}()
	go func() {
		if err := srv.ServeAPI(); err != nil {
			log.Error("local_api_serve_failed", zap.String("socket", coagenthome.APISocketFileName), zap.Error(err))
		}
	}()

	a.onStop("localapi", srv.Close)

	return srv, nil
}

func attachConfiguredAPI(
	ctx context.Context,
	srv *localapi.Server,
	cfg *config.Config,
	controllers controllerapi.ManagerControllerFactory,
) {
	if cfg.UnifiedConfig == nil || len(cfg.UnifiedConfig.API.Clients) == 0 {
		return
	}

	log := logger.Ctx(ctx).Named("main.localapi")

	path, err := coagenthome.Join(coagenthome.APISocketFileName)
	if err != nil {
		log.Error("local_api_start_failed", zap.Error(err))
		return
	}

	listener, err := localapi.ListenUnix(ctx, path)
	if err != nil {
		log.Error("local_api_start_failed", zap.Error(err))
		return
	}

	clients := make([]localapi.Client, 0, len(cfg.UnifiedConfig.API.Clients))
	for _, entry := range cfg.UnifiedConfig.API.Clients {
		var control controllerapi.Controller

		switch entry.Role {
		case config.APIRoleOperator:
			control = controllers.ForManager(entry.ID)
		case config.APIRoleApp:
			control = controllers.ForApp(entry.ID, entry.Projects)
		default:
			log.Error("local_api_client_refused", zap.String("client_id", entry.ID), zap.String("role", entry.Role))
			continue
		}

		clients = append(clients, localapi.Client{
			ID: entry.ID, TokenSHA256: entry.TokenSHA256, Control: control,
		})
	}

	if err := srv.AttachAPI(listener, clients); err != nil {
		_ = listener.Close()

		log.Error("local_api_start_failed", zap.Error(err))

		return
	}

	attachConfiguredLoopback(ctx, srv, cfg.UnifiedConfig.API)
}

func attachConfiguredLoopback(ctx context.Context, srv *localapi.Server, api config.APIConfig) {
	if api.Listen == "" {
		return
	}

	log := logger.Ctx(ctx).Named("main.localapi")

	listener, err := localapi.ListenLoopback(ctx, api.Listen)
	if err != nil {
		log.Error("local_api_start_failed", zap.String("socket", api.Listen), zap.Error(err))
		return
	}

	srv.AttachLoopback(listener, api.Origins)
	go func() {
		if err := srv.ServeLoopback(); err != nil {
			log.Error("local_api_serve_failed", zap.String("socket", api.Listen), zap.Error(err))
		}
	}()
}
