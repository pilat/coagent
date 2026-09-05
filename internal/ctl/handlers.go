package ctl

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
)

// mutatingOps change daemon state and are worth an info line each.
var mutatingOps = map[string]bool{
	OpSetProvider: true,
	OpSetSecret:   true,
}

func (s *Server) dispatch(ctx context.Context, c *Conn, line []byte) Response {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, CodeParse, "malformed request: "+err.Error())
	}

	if req.Method == "" {
		return errorResponse(req.ID, CodeInvalidRequest, "method is required")
	}

	result, rpcErr := s.call(ctx, c, req)

	logCall(req, rpcErr)

	if rpcErr != nil {
		return Response{JSONRPC: jsonrpcVersion, ID: req.ID, Error: rpcErr}
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return errorResponse(req.ID, CodeInternal, "encode result: "+err.Error())
	}

	return Response{JSONRPC: jsonrpcVersion, ID: req.ID, Result: encoded}
}

func (s *Server) call(ctx context.Context, c *Conn, req Request) (any, *Error) {
	// One answer for every op while booting, status included: the managers it
	// would report have not started, so "running: false" would be a lie.
	if !s.isReady() {
		return nil, &Error{Code: CodeStarting, Message: "daemon is starting"}
	}

	if req.Method == OpStatus {
		return s.status(ctx), nil
	}

	h, ok := s.handler(req.Method)
	if !ok {
		return nil, &Error{Code: CodeMethodNotFound, Message: "unknown method " + req.Method}
	}

	return h(ctx, c, req.Params)
}

func (s *Server) status(ctx context.Context) StatusResult {
	cfg := s.deps.Config

	out := StatusResult{
		BinaryVersion:   s.version,
		ProtocolVersion: ProtocolVersion,
		BootID:          s.bootID,
		PID:             os.Getpid(),
		UptimeSeconds:   int64(time.Since(s.started).Seconds()),
		ConfigPath:      s.deps.ConfigPath,
		ConfigPresent:   cfg != nil && cfg.UnifiedConfig != nil,
	}

	if out.ConfigPresent {
		out.Providers = providerStatuses(cfg.UnifiedConfig.Providers)
		out.ModelCount = len(cfg.UnifiedConfig.Models)
		out.DefaultModel = cfg.DefaultModel()
		out.Search = searchStatus(cfg.UnifiedConfig, cfg.DefaultModel())
		out.Managers = s.managerStatuses(ctx, cfg.UnifiedConfig.Managers)
	}

	if s.deps.Builtin != nil {
		builtin := ManagerStatus{ID: s.deps.Builtin.ID(), Driver: "cli", Enabled: true, Running: s.deps.Builtin.Alive()}
		if s.deps.Delivery != nil {
			health, err := s.deps.Delivery.OutputQueueStatus(ctx, builtin.ID)
			if err == nil {
				builtin.PendingOutputs = health.Pending
				builtin.BlockedOutputID = health.BlockedID
				builtin.BlockedForSeconds = health.BlockedForSec
				builtin.DeliveryError = health.DeliveryError
			}
		}

		out.Managers = append(out.Managers, builtin)
	}

	s.appendRemovedManagerBacklogs(ctx, &out)

	return out
}

func (s *Server) appendRemovedManagerBacklogs(ctx context.Context, out *StatusResult) {
	owners, ok := s.deps.Delivery.(controllerapi.OutputOwnerStatusFactory)
	if !ok {
		return
	}

	ids, err := owners.UnresolvedOutputOwners(ctx)
	if err != nil {
		return
	}

	known := make(map[string]struct{}, len(out.Managers))
	for _, manager := range out.Managers {
		known[manager.ID] = struct{}{}
	}

	for _, id := range ids {
		if _, exists := known[id]; exists {
			continue
		}

		manager := ManagerStatus{ID: id, Driver: "removed"}

		health, err := s.deps.Delivery.OutputQueueStatus(ctx, id)
		if err == nil {
			manager.PendingOutputs = health.Pending
			manager.BlockedOutputID = health.BlockedID
			manager.BlockedForSeconds = health.BlockedForSec
			manager.DeliveryError = health.DeliveryError
		}

		out.Managers = append(out.Managers, manager)
	}
}

func (s *Server) managerStatuses(ctx context.Context, entries []config.ManagerEntry) []ManagerStatus {
	running := map[string]struct{}{}

	if s.deps.Managers != nil {
		for _, id := range s.deps.Managers.RunningIDs() {
			running[id] = struct{}{}
		}
	}

	out := make([]ManagerStatus, 0, len(entries))

	for _, m := range entries {
		_, isRunning := running[m.ID]

		st := ManagerStatus{
			ID:      m.ID,
			Driver:  m.Driver,
			Enabled: m.Enabled != nil && *m.Enabled,
			Running: isRunning,
		}
		if s.deps.Delivery != nil {
			health, err := s.deps.Delivery.OutputQueueStatus(ctx, m.ID)
			if err == nil {
				st.PendingOutputs = health.Pending
				st.BlockedOutputID = health.BlockedID
				st.BlockedForSeconds = health.BlockedForSec
				st.DeliveryError = health.DeliveryError
			}
		}

		if st.Enabled && !isRunning {
			st.Error = s.managerError(m.ID)
		}

		out = append(out, st)
	}

	return out
}

// managerError is why this manager is down — its own reason, never a sibling's.
func (s *Server) managerError(id string) string {
	if s.deps.Managers == nil {
		return ""
	}

	err := s.deps.Managers.StartError(id)
	if err == nil {
		return ""
	}

	return logger.Redact(err.Error())
}

func providerStatuses(providers map[string]config.ProviderEntry) []ProviderStatus {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}

	slices.Sort(names)

	out := make([]ProviderStatus, 0, len(names))
	for _, name := range names {
		out = append(out, ProviderStatus{Name: name, Driver: providers[name].Driver})
	}

	return out
}

// searchStatus renders the integrated-search state for `coagent status`. It
// mirrors the session-side precedence: an explicit REST provider beats native
// passthrough, an explicit disable beats everything.
func searchStatus(unified *config.UnifiedConfig, defaultModel string) string {
	search := unified.Tools.Search

	switch {
	case search.SearchDisabled():
		return "disabled"
	case search.Provider == config.SearchProviderTavily:
		return "tavily"
	case search.Provider == config.SearchProviderSearxng:
		return "searxng (" + search.BaseURL + ")"
	}

	if unified.SearchNativeActive(defaultModel) {
		return "native (openrouter)"
	}

	return ""
}

// logCall records every control op so a CLI action is traceable from the daemon
// side. Params are never logged: they carry credentials. Mutations log at info;
// the reads a client polls on a timer stay at debug so the journal is not
// drowned.
func logCall(req Request, rpcErr *Error) {
	log := logger.Named("ctl")

	if rpcErr != nil {
		log.Info("ctl_request",
			zap.String("method", req.Method),
			zap.Int("code", rpcErr.Code),
			zap.String("detail", rpcErr.Message))

		return
	}

	if mutatingOps[req.Method] {
		log.Info("ctl_request", zap.String("method", req.Method))

		return
	}

	log.Debug("ctl_request", zap.String("method", req.Method))
}

func errorResponse(id json.RawMessage, code int, message string) Response {
	return Response{JSONRPC: jsonrpcVersion, ID: id, Error: &Error{Code: code, Message: message}}
}
