package lsp

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

type diagnosticObservation struct {
	diagnostics []Diagnostic
	version     diagnosticVersion
}

func (c *client) handleNotification(ctx context.Context, notif *Notification) {
	if notif.Method != "textDocument/publishDiagnostics" {
		return
	}

	var params PublishDiagnosticsParams
	if err := json.Unmarshal(notif.Params, &params); err != nil {
		logger.Ctx(ctx).Debug("publishDiagnostics: parse error", zap.Error(err))
		return
	}

	eventGeneration := c.observeDiagnostic(params.URI)
	c.fileMu.Lock()
	state, open := c.files[params.URI]
	c.fileMu.Unlock()

	c.diagnosticsMu.Lock()
	c.initializeDiagnostics()

	oldCount := len(c.diagnostics[params.URI])
	if c.rejectDiagnosticVersion(params, state, open) {
		c.diagnosticsMu.Unlock()
		return
	}

	c.diagnostics[params.URI] = params.Diagnostics
	if !params.version.present {
		delete(c.diagVersions, params.URI)
		c.versionlessGen[params.URI] = eventGeneration
	} else {
		c.diagVersions[params.URI] = params.version
	}

	close(c.diagSignal)
	c.diagSignal = make(chan struct{})
	newCount := len(params.Diagnostics)
	c.diagnosticsMu.Unlock()

	logger.Ctx(ctx).Info("publishDiagnostics",
		zap.String("uri", params.URI),
		zap.Any("version", params.version),
		zap.Int("count", newCount),
		zap.Int("oldCount", oldCount),
	)
	logger.Ctx(ctx).Debug("publishDiagnostics: diagnostics",
		zap.String("uri", params.URI),
		zap.Any("diagnostics", params.Diagnostics),
	)
}

func (c *client) initializeDiagnostics() {
	if c.diagnostics == nil {
		c.diagnostics = make(map[string][]Diagnostic)
	}

	if c.diagVersions == nil {
		c.diagVersions = make(map[string]diagnosticVersion)
	}

	if c.staleDiags == nil {
		c.staleDiags = make(map[string]diagnosticObservation)
	}

	if c.diagnosticGen == nil {
		c.diagnosticGen = make(map[string]uint64)
	}

	if c.versionlessGen == nil {
		c.versionlessGen = make(map[string]uint64)
	}

	if c.diagSignal == nil {
		c.diagSignal = make(chan struct{})
	}
}

func (c *client) awaitDiagnostics(ctx context.Context, document documentSync) ([]Diagnostic, error) {
	if !document.changed {
		return c.getDiagnostics(document.uri), nil
	}

	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for {
		diagnostics, version, versionlessGeneration, signal := c.diagnosticsSnapshot(document.uri)
		if version.present && version.value == document.version {
			return diagnostics, nil
		}

		if !version.present && versionlessGeneration > document.generation {
			return diagnostics, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			logger.Ctx(ctx).Debug("LSP diagnostics timed out", zap.String("uri", document.uri))
			return c.getDiagnostics(document.uri), nil
		case <-signal:
		}
	}
}

func (c *client) diagnosticsSnapshot(uri string) ([]Diagnostic, diagnosticVersion, uint64, <-chan struct{}) {
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()

	diagnostics := append([]Diagnostic(nil), c.diagnostics[uri]...)

	return diagnostics, c.diagVersions[uri], c.versionlessGen[uri], c.diagSignal
}

func (c *client) observeDiagnostic(uri string) uint64 {
	c.diagnosticsMu.Lock()
	defer c.diagnosticsMu.Unlock()

	c.initializeDiagnostics()
	c.diagnosticGen[uri]++

	return c.diagnosticGen[uri]
}

func (c *client) rejectDiagnosticVersion(
	params PublishDiagnosticsParams,
	state documentState,
	open bool,
) bool {
	if !params.version.present || !open {
		return false
	}

	if params.version.value == state.version {
		return false
	}

	if params.version.value < state.version {
		c.staleDiags[params.URI] = diagnosticObservation{
			diagnostics: append([]Diagnostic(nil), params.Diagnostics...),
			version:     params.version,
		}

		return true
	}

	return true
}
