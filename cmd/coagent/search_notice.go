package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

// An explicit search choice and native-capable models suppress the omission notice.
func searchUnconfigured(unified *config.UnifiedConfig) bool {
	if unified == nil {
		return false // no config yet; nothing configured search
	}

	search := unified.Tools.Search
	if search.SearchActive() || search.SearchDisabled() {
		return false
	}

	for _, m := range unified.Models {
		if unified.SearchNativeActive(m.ID) {
			return false
		}
	}

	return true
}

// noticeSearchUnconfigured logs the one-time discoverability hint. Info, not
// Warn: the absence of search is not a malfunction.
func noticeSearchUnconfigured(ctx context.Context, cfg *config.Config) {
	if !searchUnconfigured(cfg.UnifiedConfig) {
		return
	}

	logger.Ctx(ctx).Named("daemon.search_notice").Info("search_not_configured",
		zap.String("hint",
			"sessions have no web search; set tools.search.provider (tavily or searxng) in config.yaml, "+
				"or use an openrouter-driver model for native search"))
}
