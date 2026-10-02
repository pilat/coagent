package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/daemon"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managercontrol"
	"github.com/pilat/coagent/internal/managers"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/version"
)

// catalogEnrichTimeout bounds the whole enrichment pass; individual catalog
// fetches carry their own shorter deadline.
const catalogEnrichTimeout = 30 * time.Second

// errRestartRequested is how runDaemon reports "come back on the new config".
// It is not a failure: bootDaemon execs once the deferred drain has released the
// database and the socket.
var errRestartRequested = errors.New("restart requested")

// errUnsupportedPlatform is the stable refusal for a binary cross-compiled to
// any OS other than Linux. It fires before the guardian, configuration, sandbox
// probing, service handling, or dispatch, and is independent of sandbox
// configuration.
var errUnsupportedPlatform = errors.New("coagent supports Linux only")

// ensureLinuxPlatform rejects a non-Linux build before it can reach any
// process-lifecycle behavior. The platform is injected at the entry seam so the
// refusal ordering is directly testable rather than implied by source order.
func ensureLinuxPlatform(goos string) error {
	if goos == "linux" {
		return nil
	}

	return fmt.Errorf("%w: refusing a binary built for %q; rebuild for linux", errUnsupportedPlatform, goos)
}

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
	return runWith(
		runtime.GOOS,
		os.Args[1:],
		runModes(
			backgroundprocess.RunGuardian,
		),
		dispatch,
	)
}

// runModes chains the hidden mode entry points: the first one that recognizes
// the invocation handles it.
func runModes(modes ...func(args []string) (bool, error)) func(args []string) (bool, error) {
	return func(args []string) (bool, error) {
		for _, mode := range modes {
			if handled, err := mode(args); handled {
				return true, err
			}
		}

		return false, nil
	}
}

// runWith is the entry seam with the platform injected. The platform guard is
// the first operation: a non-Linux binary must gain no process-lifecycle
// behavior — not even guardian execution — before it refuses.
func runWith(
	goos string,
	args []string,
	guardian func(args []string) (bool, error),
	disp func(ctx context.Context, args []string) int,
) int {
	if err := ensureLinuxPlatform(goos); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)

		return exitError
	}

	if handled, err := guardian(args); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)

			return exitError
		}

		return exitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return disp(ctx, args)
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

	fields := []zap.Field{
		zap.Bool("applied", outcome.Verdict.Applied),
		zap.Bool("rolled_back", outcome.RolledBack),
		zap.Int64("session_id", outcome.Pending.SessionID),
	}
	if bootErr != nil {
		fields = append(fields, zap.String("boot_error", logger.Redact(bootErr.Error())))
	}

	log.Info("pending_apply_resolved", fields...)

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
		zap.Bool("sandbox_enabled", cfg.UnifiedConfig.Sandbox.Enabled),
		zap.Int("sandbox_global_rules", len(cfg.UnifiedConfig.Sandbox.Rules)),
		zap.Int("sandbox_projects", len(cfg.UnifiedConfig.Sandbox.Projects)),
	)

	// coagent ships no built-in deny rule: an allow-only configuration is a
	// legitimate operator choice, not a default. This warns so nobody assumes
	// the boundary withholds credentials when it does not.
	if cfg.UnifiedConfig.Sandbox.Enabled && !sandboxHasDenyRule(cfg.UnifiedConfig.Sandbox) {
		log.Warn("sandbox enabled with no deny rules: every file the daemon user can read is readable by sessions")
	}
}

func sandboxHasDenyRule(section sandboxpolicy.Section) bool {
	for _, rule := range section.Rules {
		if rule.Deny != "" {
			return true
		}
	}

	for _, project := range section.Projects {
		for _, rule := range project.Rules {
			if rule.Deny != "" {
				return true
			}
		}
	}

	return false
}

// probeBashSandbox fails startup when Bash confinement is configured but the
// platform backend cannot enforce it, so sessions never run unconfined.
func probeBashSandbox(cfg *config.Config) error {
	if cfg.UnifiedConfig == nil || !cfg.UnifiedConfig.Sandbox.Enabled {
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

	core, err := startCore(ctx, a, cfg, secrets, ops)
	if err != nil {
		return err
	}

	executor := schedule.NewExecutor(core.scheduleStore, core.scheduleSender)
	executor.Start(ctx)

	a.onStop("schedule.executor", func(context.Context) error { executor.Stop(); return nil })

	mgrRuntime := managers.NewRuntime(cfg, core.controller)

	deliveryStatus, _ := core.controller.(controllerapi.OutputStatusFactory)

	ctlSrv, err := prepareControlSocket(ctx, cfg, mgrRuntime, deliveryStatus)
	if err != nil {
		return err
	}

	a.onStop("ctl.server", func(context.Context) error { return ctlSrv.Close() })

	// Answering starts with the bind, not with readiness: connect success is the
	// liveness test, so a bound socket nobody answers reads as a broken daemon.
	serveControlSocket(ctx, ctlSrv)

	if err := mgrRuntime.Start(ctx); err != nil {
		return fmt.Errorf("start managers: %w", err)
	}

	a.onStop("managers", mgrRuntime.Stop)
	ctlSrv.MarkReady()

	deliverApplyVerdict(ctx, core.sessionStore, core.applier, ops, outcome)

	select {
	case <-ctx.Done():
		return nil
	case <-core.applier.Restart():
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
// session came from an unattended apply, which already had its answer.
func deliverApplyVerdict(
	ctx context.Context,
	sender sessionstore.Store,
	applier configapply.Service,
	ops configops.Service,
	outcome *configops.Outcome,
) {
	if outcome == nil {
		return
	}

	log := logger.Named("main.apply")

	if outcome.Pending.SessionID != 0 {
		// An applied verdict confirms the commit, so spend the /config grant the
		// apply's own process may have died before spending. A failed verdict is
		// left to the loop's terminal settlement — nothing was kept, so its
		// "was not changed" receipt is true.
		if !outcome.Verdict.Failed() {
			applier.ConsumeConfigEditActivation(
				ctx, outcome.Pending.SessionID, outcome.Pending.ToolCallID,
			)
		}

		message := "Config applied: " + outcome.Pending.Summary
		if outcome.Verdict.Failed() {
			message = "Config change rejected — " + outcome.Verdict.Reason()
		}

		_, err := sender.Enqueue(ctx, sessionstore.Input{
			SessionID: outcome.Pending.SessionID, Source: sessionstore.InputSourceCallResult, Content: message,
			Attributes:  map[string]any{"call_id": outcome.Pending.ToolCallID, "tool_id": outcome.Pending.ToolName},
			DeliveryKey: "config_apply:" + outcome.Pending.ToolCallID,
		})

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
func verdictUndeliverable(ctx context.Context, sender sessionstore.Store, sessionID int64) bool {
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
	sessionStore   sessionstore.Store
	applier        configapply.Service
}

func startCore(
	ctx context.Context,
	a *app,
	cfg *config.Config,
	secrets config.Secrets,
	ops configops.Service,
) (*core, error) {
	gitClient := git.New()

	marketplaceGitClient, err := newMarketplaceGitClient(ctx, cfg)
	if err != nil {
		return nil, err
	}

	cache := loader.NewMarketplaceCache(marketplaceGitClient)

	db, err := migrate.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	a.onStop("db", func(context.Context) error { return db.Close() })

	daemonStore := daemon.NewStore(db)
	sessionStore := sessionstore.NewStore(db)
	scheduleStore := schedule.NewStore(db, sessionStore)
	curatedStore := memory.NewCuratedStore(db)
	linkStore := subagent.NewStore(db)
	subagentTx := subagent.NewTransactions(db, sessionStore)
	applier := configapply.New(ops, sessionStore)

	budgetSvc := budget.New(budget.Store(sessionStore))
	mcpRegistry := mcpstore.NewStore(db)

	if _, err := sessionStore.RecoverInterruptedOutputs(ctx); err != nil {
		return nil, fmt.Errorf("recover interrupted manager output: %w", err)
	}

	scheduleSvc := schedule.NewService(scheduleStore)

	factory := session.NewFactoryWithOptions(
		cfg, secrets, curatedStore, sessionStore, sessionStore,
		gitClient, mcpRegistry, cache,
	)

	daemonSvc := daemon.New(
		ctx, factory, daemonStore, sessionStore, sessionStore, sessionStore,
		sessionStore, sessionStore, sessionStore, sessionStore,
		linkStore, subagentTx, budgetSvc, sessionStore,
		scheduleSvc, cfg, mcpRegistry, applier,
	)

	controller := managercontrol.New(daemonSvc, daemonSvc, sessionStore, cfg, cache)

	if err := daemonSvc.Start(ctx); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}

	a.onStop("daemon", func(context.Context) error { daemonSvc.Shutdown(30 * time.Second); return nil })

	return &core{
		controller:     controller,
		scheduleStore:  scheduleStore,
		scheduleSender: daemonSvc,
		sessionStore:   sessionStore,
		applier:        applier,
	}, nil
}

func newMarketplaceGitClient(_ context.Context, cfg *config.Config) (git.Client, error) {
	if cfg == nil || cfg.UnifiedConfig == nil || !cfg.UnifiedConfig.Sandbox.Enabled {
		return git.New(), nil
	}

	marketplaceDir, err := coagenthome.Join(coagenthome.CacheDirName, coagenthome.MarketplacesDirName)
	if err != nil {
		return nil, fmt.Errorf("resolve marketplace cache directory: %w", err)
	}

	if err := os.MkdirAll(marketplaceDir, 0o700); err != nil {
		return nil, fmt.Errorf("create marketplace cache directory: %w", err)
	}

	policy, err := marketplacePolicy(cfg.UnifiedConfig, marketplaceDir)
	if err != nil {
		return nil, fmt.Errorf("compile marketplace sandbox policy: %w", err)
	}

	//nolint:contextcheck // Sandbox preflight owns a bounded process-wide context.
	runner, err := bashsandbox.New(bashsandbox.Config{
		Enabled:    true,
		Policy:     policy,
		WorkDir:    marketplaceDir,
		SessionKey: "marketplace",
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("create marketplace sandbox: %w", err)
	}

	return git.NewSandboxed(procexec.Runner(runner)), nil
}

// marketplacePolicy confines the daemon's own marketplace Git operations. It is
// an operator-owned action rather than a session, so it takes the operator's
// global rules and no project rules of its own.
func marketplacePolicy(unified *config.UnifiedConfig, dir string) (sandboxpolicy.Policy, error) {
	return sandboxpolicy.Compile(sandboxpolicy.Request{
		ProjectRoot: dir, WorkDir: dir, GlobalRules: unified.Sandbox.Rules,
	})
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

// prepareControlSocket binds the status-only control socket. Readiness is marked
// only once the managers register theirs, so a client sees "starting", not unknown op.
func prepareControlSocket(
	ctx context.Context,
	cfg *config.Config,
	mgrs ctl.ManagerControl,
	delivery controllerapi.OutputStatusFactory,
) (*ctl.Server, error) {
	path, err := ctl.SocketPath()
	if err != nil {
		return nil, err
	}

	srv, err := ctl.NewServer(ctx, path, version.Version, ctl.Deps{
		Config:     cfg,
		ConfigPath: config.DefaultUnifiedConfigFile,
		Managers:   mgrs,
		Delivery:   delivery,
	})
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
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
