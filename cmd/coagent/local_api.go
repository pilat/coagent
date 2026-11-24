package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/localapi"
	"github.com/pilat/coagent/internal/logger"
)

func startLocalAPI(
	ctx context.Context,
	a *app,
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

	listener, err := ctl.ListenUnix(ctx, path)
	if err != nil {
		log.Error("local_api_start_failed", zap.Error(err))
		return
	}

	clients := make([]localapi.Client, 0, len(cfg.UnifiedConfig.API.Clients))
	for _, entry := range cfg.UnifiedConfig.API.Clients {
		var control controllerapi.Controller
		if entry.Role == config.APIRoleApp {
			control = controllers.ForApp(entry.ID, entry.Projects)
		} else {
			control = controllers.ForManager(entry.ID)
		}

		clients = append(clients, localapi.Client{
			ID: entry.ID, TokenSHA256: entry.TokenSHA256, Control: control,
		})
	}

	srv, err := localapi.New(ctx, listener, clients)
	if err != nil {
		_ = listener.Close()

		log.Error("local_api_start_failed", zap.Error(err))

		return
	}

	go func() {
		if err := srv.Serve(); err != nil {
			log.Error("local_api_serve_failed", zap.Error(err))
		}
	}()

	a.onStop("localapi", srv.Close)
}
