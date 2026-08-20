package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/daemon"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managers"
	"github.com/pilat/coagent/internal/managers/cli"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/shellenv"
	"github.com/pilat/coagent/internal/version"
)

// catalogEnrichTimeout bounds the whole enrichment pass; individual catalog
// fetches carry their own shorter deadline.
const catalogEnrichTimeout = 30 * time.Second

// errRestartRequested is how runDaemon reports "come back on the new config".
// It is not a failure: bootDaemon execs once the deferred drain has released the
// database and the socket.
var errRestartRequested = errors.New("restart requested")

// selfExecPath is where this binary lives, resolved at process start. It must be
// captured before anything can swap the file: after an update /proc/self/exe
// reads as "… (deleted)", while this path holds the new binary — which is
// exactly what the restart should exec.
var selfExecPath = resolveSelfExecPath()

func resolveSelfExecPath() string {
	path, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}

	return path
}

type namedStop struct {
	name string
	fn   func(context.Context) error
}

type app struct {
	stops []namedStop
}

func (a *app) onStop(name string, fn func(context.Context) error) {
	a.stops = append(a.stops, namedStop{name: name, fn: fn})
}

func (a *app) shutdown(ctx context.Context) {
	log := logger.Named("main.shutdown")

	for _, v := range slices.Backward(a.stops) {
		s := v
		if err := s.fn(ctx); err != nil {
			log.Warn("component_stop_failed", zap.String("component", s.name), zap.Error(err))
		}
	}
}

func main() {
	os.Exit(run())
}

// run keeps os.Exit out of any deferred-cleanup scope: main calls it exactly
// once, after every defer in this function has already unwound.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return dispatch(ctx, os.Args[1:])
}

// startupState is everything the daemon needs and everything that can refuse to
// let it start. A pending-apply marker wraps the *whole* of it, not just the
// config parse: the failures a pre-write check cannot see — a cold catalog cache
// with models.dev unreachable, a model id that drifted out of the catalog — are
// exactly what the rollback exists for.
type startupState struct {
	cfg     *config.Config
	secrets config.Secrets
}

// bootDaemon loads configuration and runs the daemon in the foreground. This is
// what the service unit executes; every other verb is a socket client.
func bootDaemon(ctx context.Context) int {
	logger.Init(logger.WithConsoleOutput(os.Stderr), logger.WithSessionPrefix())

	ops, err := newConfigOps()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	pending, err := ops.LoadPending()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	state, bootErr := loadStartupState(ctx)

	var outcome *configops.Outcome

	if pending != nil {
		state, outcome, bootErr = resolvePendingApply(ctx, ops, *pending, state, bootErr)
	}

	// No marker and a config that will not load stays fatal, exactly as before:
	// a hand-edited breakage keeps its loud failure.
	if bootErr != nil {
		fmt.Fprintf(os.Stderr, "%s\n", logger.Redact(bootErr.Error()))
		return 1
	}

	err = runDaemon(ctx, state, ops, outcome)

	if errors.Is(err, errRestartRequested) {
		return execSelf()
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", logger.Redact(err.Error()))
		return 1
	}

	return 0
}

// resolvePendingApply decides what this boot makes of a pending-apply marker,
// and re-runs the startup validation when it had to roll back.
func resolvePendingApply(
	ctx context.Context,
	ops configops.Service,
	pending configops.Pending,
	state startupState,
	bootErr error,
) (startupState, *configops.Outcome, error) {
	outcome, err := ops.ResolvePending(pending, bootErr)
	if err != nil {
		return state, nil, err
	}

	log := logger.Named("main.apply")
	log.Info("pending_apply_resolved",
		zap.Bool("applied", outcome.Verdict.Applied),
		zap.Bool("rolled_back", outcome.RolledBack),
		zap.Int64("session_id", outcome.Pending.SessionID),
	)

	if !outcome.RolledBack {
		return state, &outcome, bootErr
	}

	state, bootErr = loadStartupState(ctx)

	return state, &outcome, bootErr
}

// loadStartupState runs every check that can refuse the boot, so a caller can
// treat "the daemon cannot start on this config" as one error.
func loadStartupState(ctx context.Context) (startupState, error) {
	cfg, secrets, err := config.NewConfig()
	if err != nil {
		return startupState{}, fmt.Errorf("config: %w", err)
	}

	logger.SetRedactedValues(cfg.SecretValues)

	logConfigStatus(cfg)

	// Model metadata comes from external catalogs, so a model whose limits cannot
	// be resolved is a config error — there is no override to fall back on.
	enrichCtx, cancelEnrich := context.WithTimeout(ctx, catalogEnrichTimeout)
	err = llm.EnrichCatalog(enrichCtx, cfg)

	cancelEnrich()

	if err != nil {
		return startupState{}, fmt.Errorf("model catalog: %w", err)
	}

	if err := probeBashSandbox(cfg); err != nil {
		return startupState{}, fmt.Errorf("bash sandbox: %w", err)
	}

	return startupState{cfg: cfg, secrets: secrets}, nil
}

func newConfigOps() (configops.Service, error) {
	configPath, err := config.ExpandPath(config.DefaultUnifiedConfigFile)
	if err != nil {
		return nil, err
	}

	secretsPath, err := config.SecretsFilePath()
	if err != nil {
		return nil, err
	}

	return configops.New(configPath, secretsPath), nil
}

// execSelf replaces this process with the binary at the path captured at start.
// After an update that path holds the *new* binary, which is the point;
// /proc/self/exe would read as "… (deleted)" instead.
func execSelf() int {
	log := logger.Named("main.restart")
	log.Info("exec_self", zap.String("path", selfExecPath))

	// The path is os.Executable() captured at boot and the argv is this process's
	// own — nothing here comes from a request.
	//nolint:gosec // G702: re-executing this same binary with its own argv
	err := syscall.Exec(selfExecPath, os.Args, os.Environ())

	// Exec only returns on failure. Exiting non-zero is the recovery: the service
	// unit's Restart=on-failure brings the daemon back.
	log.Error("exec_failed", zap.String("path", selfExecPath), zap.Error(err))

	return 1
}

// onboardingModel picks what the local chat runs on: a provider's onboarding
// recommendation when that model is actually enabled, else the daemon's default.
// The rule is unconditional — there is no "am I onboarding" signal to read, and
// the recommendation is a good chat model whether or not it is a first run.
func onboardingModel(cfg *config.Config) string {
	if cfg.UnifiedConfig == nil {
		return ""
	}

	enabled := make(map[string]bool, len(cfg.UnifiedConfig.Models))
	for _, m := range cfg.UnifiedConfig.Models {
		enabled[m.ID] = true
	}

	for _, name := range slices.Sorted(maps.Keys(cfg.UnifiedConfig.Providers)) {
		section, ok := llm.CatalogSection(cfg.UnifiedConfig.Providers[name])
		if !ok {
			continue
		}

		rec, ok := recommend(section)
		if ok && enabled[rec.Onboarding] {
			return rec.Onboarding
		}
	}

	return cfg.DefaultModel()
}

// logConfigStatus reports the unified-config load outcome; config itself stays
// a pure leaf and does not log.
func logConfigStatus(cfg *config.Config) {
	log := logger.Named("main")

	if cfg.UnifiedConfig == nil {
		log.Info("no config file", zap.String("path", config.DefaultUnifiedConfigFile))
		return
	}

	log.Info("config loaded",
		zap.Int("marketplaces", len(cfg.UnifiedConfig.Marketplaces)),
		zap.Bool("bash_sandbox_enabled", cfg.UnifiedConfig.Tools.Bash.Sandbox.Enabled),
	)
}

// probeBashSandbox fails startup when Bash confinement is configured but the
// platform backend cannot enforce it, so sessions never run unconfined.
func probeBashSandbox(cfg *config.Config) error {
	if cfg.UnifiedConfig == nil || !cfg.UnifiedConfig.Tools.Bash.Sandbox.Enabled {
		return nil
	}

	return bashsandbox.Probe()
}

func runDaemon(
	ctx context.Context,
	state startupState,
	ops configops.Service,
	outcome *configops.Outcome,
) error {
	cfg, secrets := state.cfg, state.secrets
	a := &app{}
	// Stop order is the reverse of registration; each onStop below records its own.
	//nolint:contextcheck // shutdown runs after ctx is already canceled; needs its own bounded deadline
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		a.shutdown(stopCtx)
	}()

	// The single-instance guard comes before the database: two daemons on one
	// SQLite file under WAL corrupt each other silently.
	lock, err := acquireInstanceLock()
	if err != nil {
		return err
	}

	a.onStop("ctl.lock", func(context.Context) error { return lock.Release() })

	restart := make(chan struct{}, 1)
	applier := daemon.NewConfigApplier(ops, func() {
		select {
		case restart <- struct{}{}:
		default: // a restart is already on its way
		}
	})

	core, err := startCore(ctx, a, cfg, secrets, applier)
	if err != nil {
		return err
	}

	executor := schedule.NewExecutor(core.scheduleStore, core.scheduleSender)
	executor.Start(ctx)

	a.onStop("schedule.executor", func(context.Context) error { executor.Stop(); return nil })

	runtime := managers.NewRuntime(cfg, core.controller)

	ctlSrv, err := prepareControlSocket(ctx, cfg, runtime, applier, core.secretResolver)
	if err != nil {
		return err
	}

	a.onStop("ctl.server", func(context.Context) error { return ctlSrv.Close() })

	// Answering starts with the bind, not with readiness: connect success is the
	// liveness test, so a bound socket nobody answers reads as a broken daemon.
	serveControlSocket(ctx, ctlSrv)

	// The local chat is built and started outside the config-driven loop: it is
	// how a daemon with no config gets one, so it cannot be a config entry.
	// The chat sees only its own contract type, never main's wider resolver.
	var secretPushes cli.SecretRequests = core.secretResolver

	chat := cli.New(
		core.controller.ForManager(controllerapi.BuiltinCLIManagerID),
		ctlSrv,
		onboardingModel(cfg),
		secretPushes,
	)
	if err := chat.Start(ctx); err != nil {
		return fmt.Errorf("start local chat: %w", err)
	}

	a.onStop("managers.cli", chat.Stop)

	if err := runtime.Start(ctx); err != nil {
		return fmt.Errorf("start managers: %w", err)
	}

	a.onStop("managers", runtime.Stop)
	ctlSrv.MarkReady()

	deliverApplyVerdict(ctx, core.verdictSender, ops, outcome)

	select {
	case <-ctx.Done():
		return nil
	case <-restart:
		// The deferred shutdown replays every stop closure before this returns;
		// bootDaemon execs only after that drain, so the new image starts against
		// a released database and socket.
		return errRestartRequested
	}
}

// deliverApplyVerdict answers the tool call that survived the restart, then
// clears the marker. A delivery that failed keeps it: the marker is the only
// record of a session suspended on a config call, and the next boot re-delivers
// against a transcript where a second result is a no-op. A marker with no
// session came from a bootstrap op, which already had its answer over the socket.
func deliverApplyVerdict(
	ctx context.Context,
	sender applyVerdictSender,
	ops configops.Service,
	outcome *configops.Outcome,
) {
	if outcome == nil {
		return
	}

	log := logger.Named("main.apply")

	if outcome.Pending.SessionID != 0 {
		message := "Config applied: " + outcome.Pending.Summary
		if outcome.Verdict.Failed() {
			message = "Config change rejected — " + outcome.Verdict.Reason()
		}

		_, err := sender.DeliverPendingCallResult(
			ctx,
			outcome.Pending.SessionID,
			outcome.Pending.ToolCallID,
			outcome.Pending.ToolName,
			message,
		)

		if err != nil && !verdictUndeliverable(ctx, sender, outcome.Pending.SessionID) {
			log.Error("verdict_delivery_failed",
				zap.Int64("session_id", outcome.Pending.SessionID), zap.Error(err))

			return
		}

		if err != nil {
			log.Error("verdict_undeliverable",
				zap.Int64("session_id", outcome.Pending.SessionID),
				zap.String("summary", outcome.Pending.Summary),
				zap.Bool("rolled_back", outcome.RolledBack),
				zap.Error(err))
		}
	}

	if err := ops.ClearPending(outcome.Pending); err != nil {
		log.Error("clear_pending_apply_marker", zap.Error(err))
	}
}

// verdictUndeliverable reports whether the owed session can never take the
// verdict — a marker no boot can consume arms every later one to roll back.
func verdictUndeliverable(ctx context.Context, sender applyVerdictSender, sessionID int64) bool {
	rec, err := sender.GetSession(ctx, sessionID)
	if err != nil || rec == nil {
		return true
	}

	return rec.KilledAt != nil ||
		rec.Status == sessionstore.SessionStatusKilled ||
		rec.Status == sessionstore.SessionStatusStopping ||
		rec.Status == sessionstore.SessionStatusStopped
}

// core is what the control plane is wired onto. Every field names the exact
// capability runDaemon hands to a consumer; it is a return value, not a layer.
type core struct {
	controller     controllerapi.ManagerControllerFactory
	scheduleStore  schedule.Store
	scheduleSender schedule.SessionSender
	verdictSender  applyVerdictSender
	secretResolver secretRequestResolver
}

// applyVerdictSender is what verdict delivery needs — delivery plus the session
// state separating "not now" from "never" — so it is testable without a daemon.
type applyVerdictSender interface {
	DeliverPendingCallResult(
		ctx context.Context, sessionID int64, callID, toolName, content string,
	) (bool, error)
	GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error)
}

// secretRequestResolver is the masked-prompt lifecycle, kept separate so an RPC
// handler cannot acquire the full session-control surface by accident. It names
// cli.SecretRequests' methods structurally: main's type must not flow into the chat.
type secretRequestResolver interface {
	PendingSecretRequests(sessionID int64) []sessionevent.Notification
	CancelSecretRequest(ctx context.Context, requestID string) error
	ResolveSecretRequest(ctx context.Context, requestID, name string) error
}

// startCore brings up everything below the control plane — shell activation, the
// MCP pool, the database, the session factory and the daemon — registering each
// component's stop closure the moment it exists.
func startCore(
	ctx context.Context,
	a *app,
	cfg *config.Config,
	secrets config.Secrets,
	applier *daemon.ConfigApplier,
) (*core, error) {
	gitClient := git.New()

	provider := shellenv.New()

	a.onStop("shellenv", func(context.Context) error { return provider.Close() })

	pool := mcp.NewPool(provider)

	a.onStop("mcp.pool", func(context.Context) error { pool.Stop(); return nil })

	cache := loader.NewMarketplaceCache(gitClient)

	db, err := migrate.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	a.onStop("db", func(context.Context) error { return db.Close() })

	daemonStore := daemon.NewStore(db)
	sessionStore := sessionstore.NewStore(db)
	scheduleStore := schedule.NewStore(db)
	curatedStore := memory.NewCuratedStore(db)
	linkStore := daemon.NewLinkStore(db)
	mcpRegistry := mcpstore.NewStore(db)

	scheduleSvc := schedule.NewService(scheduleStore)

	factory := session.NewFactory(
		cfg, secrets, curatedStore, sessionStore, gitClient, pool, mcpRegistry, cache, provider,
	)

	daemonSvc := daemon.New(
		factory, daemonStore, sessionStore, sessionStore, linkStore, scheduleSvc, cfg, mcpRegistry, pool, applier,
	)
	if err := daemonSvc.Start(ctx); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}

	a.onStop("daemon", func(context.Context) error { daemonSvc.Shutdown(30 * time.Second); return nil })

	return &core{
		controller:     daemon.NewController(daemonSvc, cfg, cache, scheduleSvc),
		scheduleStore:  scheduleStore,
		scheduleSender: daemonSvc,
		verdictSender:  daemonSvc,
		secretResolver: daemonSvc,
	}, nil
}

func acquireInstanceLock() (*ctl.Lock, error) {
	path, err := ctl.LockPath()
	if err != nil {
		return nil, err
	}

	lock, err := ctl.Acquire(path)
	if err != nil {
		return nil, fmt.Errorf("single-instance lock: %w", err)
	}

	return lock, nil
}

// prepareControlSocket binds and registers the core ops. Readiness is marked only
// once the managers register theirs, so a client sees "starting", not unknown op.
func prepareControlSocket(
	ctx context.Context,
	cfg *config.Config,
	mgrs ctl.ManagerControl,
	applier *daemon.ConfigApplier,
	resolver secretRequestResolver,
) (*ctl.Server, error) {
	path, err := ctl.SocketPath()
	if err != nil {
		return nil, err
	}

	srv, err := ctl.NewServer(ctx, path, version.Version, ctl.Deps{
		Config:     cfg,
		ConfigPath: config.DefaultUnifiedConfigFile,
		Managers:   mgrs,
	})
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}

	if err := registerConfigOps(srv, applier, resolver); err != nil {
		_ = srv.Close()

		return nil, fmt.Errorf("register control ops: %w", err)
	}

	return srv, nil
}

func serveControlSocket(ctx context.Context, srv *ctl.Server) {
	go func() {
		if err := srv.ServeStarting(ctx); err != nil {
			logger.Named("ctl").Error("serve_failed", zap.Error(err))
		}
	}()
}
