package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxnet"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/tool/builtin"
)

const networkIdleTTL = 10 * time.Minute

// maxProcessDepth bounds the ancestry walk over a live process tree.
const maxProcessDepth = 64

var (
	_ builtin.NetworkOwner = (*networkOwner)(nil)
	_ builtin.NetworkLease = (*networkLease)(nil)
)

type networkOwner struct {
	mu           sync.Mutex
	entries      map[int64]*networkGeneration
	starts       map[int64]chan struct{}
	retiring     map[int64]chan struct{}
	epochs       map[int64]uint64
	revoked      map[int64]bool
	retireErrors map[int64]error
	done         chan struct{}
	stopped      bool
	start        func(context.Context, int64, sandboxpolicy.Policy) (*networkGeneration, error)
	hasWorkloads func(*networkGeneration) bool
}

type networkGeneration struct {
	rootID    int64
	digest    string
	policy    sandboxpolicy.Policy
	setup     *bashsandbox.NetworkSetup
	router    *sandboxnet.Router
	link      *bashsandbox.NetworkLink
	refs      int
	idleSince time.Time
	// reportedBusy keeps the retention notice to one line per busy stretch.
	reportedBusy bool
	closed       bool
	conns        map[net.Conn]struct{}
	mu           sync.Mutex
	stopFn       func(context.Context) error
	closeDone    chan struct{}
	closeErr     error
}

type networkLease struct {
	owner      *networkOwner
	generation *networkGeneration
	once       sync.Once
	runner     procexec.Runner
	workDir    string
}

func newNetworkOwner(ctx context.Context) *networkOwner {
	owner := &networkOwner{
		entries: make(map[int64]*networkGeneration), starts: make(map[int64]chan struct{}),
		retiring: make(map[int64]chan struct{}),
		epochs:   make(map[int64]uint64), done: make(chan struct{}),
		start:        startNetworkGeneration,
		hasWorkloads: func(g *networkGeneration) bool { return namespaceHasWorkloads(g.setup.Cmd.Process.Pid) },
	}
	go owner.reap(ctx)

	return owner
}

func probeNetworkSetup(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	project, err := os.MkdirTemp("", "coagent-network-probe-")
	if err != nil {
		return fmt.Errorf("create network probe project: %w", err)
	}

	defer func() { _ = os.RemoveAll(project) }()

	catalog, err := sandboxpolicy.Load(nil)
	if err != nil {
		return err
	}

	substrate, err := bashsandbox.ExecutionSubstrate()
	if err != nil {
		return err
	}

	tempRoot, err := coagenthome.SandboxTempDir(coagenthome.SandboxPathIdentity(project))
	if err != nil {
		return fmt.Errorf("resolve network probe storage: %w", err)
	}

	policy, err := sandboxpolicy.Compile(catalog, sandboxpolicy.Request{
		ProjectRoot: project, WorkDir: project,
		TempRoot: tempRoot, Substrate: substrate,
	})
	if err != nil {
		return fmt.Errorf("compile network probe policy: %w", err)
	}

	generation, err := startNetworkGenerationWithBinary(probeCtx, 1, policy, selfExecPath)
	if err != nil {
		return fmt.Errorf("create private network namespace and router: %w", err)
	}

	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(probeCtx), 5*time.Second)
		_ = generation.close(stopCtx)

		stopCancel()
	}()

	//nolint:contextcheck // Preflight is a bounded process-wide self-test.
	runner, err := bashsandbox.New(bashsandbox.Config{
		Enabled: true, Policy: policy, WorkDir: project,
		SessionKey: "network-probe", Network: generation.link,
	}, nil)
	if err != nil {
		return fmt.Errorf("join sandbox network: %w", err)
	}

	command, err := runner.BashCommand(probeCtx, "test -r /etc/resolv.conf && test -r /etc/hosts", project)
	if err != nil {
		return fmt.Errorf("prepare network probe command: %w", err)
	}

	output, runErr := command.CombinedOutput()
	for _, file := range command.ExtraFiles {
		_ = file.Close()
	}

	if runErr != nil {
		return fmt.Errorf("execute network probe: %w: %s", runErr, strings.TrimSpace(string(output)))
	}

	return nil
}

//nolint:funlen // This lock protects generation startup and retirement fences together.
func (o *networkOwner) Acquire(
	ctx context.Context,
	rootID int64,
	policy sandboxpolicy.Policy,
) (builtin.NetworkLease, error) {
	if rootID <= 0 {
		return nil, errors.New("network generation requires a root session")
	}

	for {
		o.mu.Lock()

		barrier, err := o.acquirableLocked(rootID)
		if err != nil {
			o.mu.Unlock()
			return nil, err
		}

		if barrier != nil {
			o.mu.Unlock()

			if waitErr := waitBarrier(ctx, barrier); waitErr != nil {
				return nil, waitErr
			}

			continue
		}

		if current := o.entries[rootID]; current != nil {
			if current.digest != policy.Digest {
				o.mu.Unlock()
				return nil, fmt.Errorf("root %d still owns another network policy", rootID)
			}

			current.mu.Lock()
			closed := current.closed
			current.mu.Unlock()

			if closed {
				o.mu.Unlock()
				return nil, errors.New("network generation is closed")
			}

			current.refs++
			o.mu.Unlock()

			return &networkLease{owner: o, generation: current}, nil
		}

		if waiting := o.starts[rootID]; waiting != nil {
			o.mu.Unlock()

			if waitErr := waitBarrier(ctx, waiting); waitErr != nil {
				return nil, waitErr
			}

			continue
		}

		o.ensureStartsLocked()

		waiting := make(chan struct{})
		o.starts[rootID] = waiting
		epoch := o.epochs[rootID]
		o.mu.Unlock()
		generation, err := o.start(ctx, rootID, policy)
		o.mu.Lock()

		stale := o.stopped || o.epochs[rootID] != epoch
		if err == nil && !stale {
			generation.refs = 1
			o.entries[rootID] = generation
		}

		if !stale || err != nil {
			delete(o.starts, rootID)
			close(waiting)
		}
		o.mu.Unlock()

		if err != nil {
			return nil, err
		}

		if stale {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			closeErr := generation.close(stopCtx)

			cancel()
			o.mu.Lock()
			if closeErr != nil {
				o.entries[rootID] = generation
				o.setRetireErrorLocked(rootID, closeErr)
			}

			delete(o.starts, rootID)
			close(waiting)
			o.mu.Unlock()

			return nil, errors.Join(errors.New("network generation was retired during startup"), closeErr)
		}

		return &networkLease{owner: o, generation: generation}, nil
	}
}

func waitBarrier(ctx context.Context, barrier <-chan struct{}) error {
	select {
	case <-barrier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func startNetworkGeneration(
	ctx context.Context,
	rootID int64,
	policy sandboxpolicy.Policy,
) (*networkGeneration, error) {
	return startNetworkGenerationWithBinary(ctx, rootID, policy, selfExecPath)
}

//nolint:funcorder // The start-map initialization belongs beside Acquire.
func (o *networkOwner) ensureStartsLocked() {
	if o.starts == nil {
		o.starts = make(map[int64]chan struct{})
	}
}

//nolint:funlen // Failure cleanup keeps every pinned descriptor paired with its owner.
func startNetworkGenerationWithBinary(
	ctx context.Context,
	rootID int64,
	policy sandboxpolicy.Policy,
	binary string,
) (_ *networkGeneration, err error) {
	setup, err := bashsandbox.StartNetworkSetup(context.WithoutCancel(ctx), bashsandbox.NetworkSetupConfig{
		Binary: binary, ReceiveTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("start network namespace: %w", err)
	}
	defer func() {
		if err != nil {
			_ = setup.Close()
			_ = setup.Cmd.Wait()
		}
	}()

	pid := setup.Cmd.Process.Pid

	userNS, err := os.Open(fmt.Sprintf("/proc/%d/ns/user", pid))
	if err != nil {
		return nil, fmt.Errorf("open user namespace: %w", err)
	}

	defer func() {
		if err != nil {
			_ = userNS.Close()
		}
	}()

	netNS, err := os.Open(fmt.Sprintf("/proc/%d/ns/net", pid))
	if err != nil {
		return nil, fmt.Errorf("open network namespace: %w", err)
	}

	defer func() {
		if err != nil {
			_ = netNS.Close()
		}
	}()

	binaryFile, err := os.Open(binary)
	if err != nil {
		return nil, fmt.Errorf("pin daemon binary: %w", err)
	}

	defer func() {
		if err != nil {
			_ = binaryFile.Close()
		}
	}()

	runtimeDir := filepath.Dir(policy.TempRoot)

	resolver, err := networkConfigFile(
		runtimeDir,
		"resolver",
		fmt.Sprintf(
			"nameserver %s\nnameserver %s\n",
			sandboxnet.DefaultLink().GatewayIPv4,
			sandboxnet.DefaultLink().GatewayIPv6,
		),
	)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			closeNetworkConfigFile(resolver)
		}
	}()

	hosts, err := networkConfigFile(
		runtimeDir,
		"hosts",
		fmt.Sprintf(
			"127.0.0.1 localhost\n::1 localhost\n%s %s\n%s %s\n",
			sandboxnet.DefaultLink().HostAliasIPv4,
			sandboxnet.HostAlias,
			sandboxnet.DefaultLink().HostAliasIPv6,
			sandboxnet.HostAlias,
		),
	)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			closeNetworkConfigFile(hosts)
		}
	}()

	options := sandboxnet.PolicyOptions{Link: sandboxnet.DefaultLink()}

	config, err := sandboxnet.ConfigFromPolicy(policy, options)
	if err != nil {
		return nil, err
	}

	router, err := sandboxnet.NewRouter(ctx, netNS, config)
	if err != nil {
		return nil, fmt.Errorf("start sandbox router: %w", err)
	}

	link := &bashsandbox.NetworkLink{
		UserNS: userNS, NetNS: netNS,
		BinaryFile: binaryFile, Resolver: resolver, Hosts: hosts,
	}

	logger.Ctx(ctx).Named("sandbox.network").Info("generation_started",
		zap.Int64("root_id", rootID), zap.String("digest", policy.Digest),
		zap.Int("holder_pid", setup.Cmd.Process.Pid))

	return &networkGeneration{
		rootID: rootID, digest: policy.Digest, policy: policy, setup: setup,
		router: router, link: link,
		conns: make(map[net.Conn]struct{}),
	}, nil
}

func networkConfigFile(dir, name, content string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create private network directory: %w", err)
	}

	file, err := os.CreateTemp(dir, name+"-")
	if err != nil {
		return nil, fmt.Errorf("create network %s: %w", name, err)
	}
	// Bubblewrap resolves bind-fd sources through /proc/self/fd and needs a named file.
	if _, err := file.WriteString(content); err != nil {
		closeNetworkConfigFile(file)
		return nil, fmt.Errorf("write network %s: %w", name, err)
	}

	return file, nil
}

func closeNetworkConfigFile(file *os.File) {
	if file == nil {
		return
	}

	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
}

func (l *networkLease) Link() *bashsandbox.NetworkLink { return l.generation.link }

func (l *networkLease) BindRunner(runner procexec.Runner, workDir string) {
	l.runner, l.workDir = runner, workDir
}

func (l *networkLease) Release() {
	l.once.Do(func() {
		l.owner.mu.Lock()
		defer l.owner.mu.Unlock()

		generation := l.generation
		if generation.refs > 0 {
			generation.refs--
		}

		if generation.refs == 0 {
			generation.idleSince = time.Time{}
			if !l.owner.hasGenerationWorkloads(generation) {
				generation.idleSince = time.Now()
			}
		}
	})
}

func (l *networkLease) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("unsupported web dial network %q", network)
	}

	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid web dial port %q", portText)
	}

	conn, err := bashsandbox.DialGuest(
		ctx,
		l.runner,
		bashsandbox.EntryBinaryPath,
		l.workDir,
		network,
		address,
	)
	if err != nil {
		return nil, err
	}

	return l.generation.trackExisting(conn)
}

func (g *networkGeneration) trackExisting(conn net.Conn) (net.Conn, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.closed {
		_ = conn.Close()
		return nil, errors.New("network generation is retired")
	}

	wrapped := &generationConn{Conn: conn, generation: g}
	g.conns[wrapped] = struct{}{}

	return wrapped, nil
}

type generationConn struct {
	net.Conn
	generation *networkGeneration
	once       sync.Once
}

func (c *generationConn) Close() error {
	var err error

	c.once.Do(func() {
		err = c.Conn.Close()
		c.generation.mu.Lock()
		delete(c.generation.conns, c)
		c.generation.mu.Unlock()
	})

	return err
}

func (o *networkOwner) Cutoff(ctx context.Context, rootID int64) error {
	o.mu.Lock()
	if o.revoked == nil {
		o.revoked = make(map[int64]bool)
	}

	o.revoked[rootID] = true
	if o.epochs == nil {
		o.epochs = make(map[int64]uint64)
	}

	o.epochs[rootID]++

	generation, starting := o.entries[rootID], o.starts[rootID]
	if generation != nil {
		generation.mu.Lock()
		generation.closed = true
		generation.mu.Unlock()
	}
	o.mu.Unlock()

	if generation != nil && generation.router != nil {
		if err := generation.router.Cutoff(); err != nil {
			return err
		}
	}

	if starting != nil {
		select {
		case <-starting:
		case <-ctx.Done():
			return fmt.Errorf("wait for revoked network startup: %w", ctx.Err())
		}
	}

	return nil
}

func (o *networkOwner) Retire(ctx context.Context, rootID int64) error {
	for {
		o.mu.Lock()
		if waiting := o.retiring[rootID]; waiting != nil {
			o.mu.Unlock()

			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if o.retiring == nil {
			o.retiring = make(map[int64]chan struct{})
		}

		barrier := make(chan struct{})

		o.retiring[rootID] = barrier
		if o.revoked == nil {
			o.revoked = make(map[int64]bool)
		}

		o.revoked[rootID] = true
		if o.epochs == nil {
			o.epochs = make(map[int64]uint64)
		}

		o.epochs[rootID]++
		generation := o.entries[rootID]
		delete(o.entries, rootID)
		delete(o.retireErrors, rootID)
		starting := o.starts[rootID]
		o.mu.Unlock()

		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := generation.close(stopCtx)

		cancel()

		timedOut := false

		if starting != nil {
			select {
			case <-starting:
			case <-time.After(15 * time.Second):
				timedOut = true
				err = errors.Join(err, errors.New("network startup did not finish during retirement"))
			}
		}

		o.mu.Lock()

		err = errors.Join(err, o.retireErrors[rootID])
		if err != nil && generation != nil {
			o.entries[rootID] = generation
		}

		if err != nil {
			logger.Ctx(ctx).Named("sandbox.network").Error("generation_retire_failed",
				zap.Int64("root_id", rootID), zap.Error(err))
			o.setRetireErrorLocked(rootID, err)
		} else {
			logger.Ctx(ctx).Named("sandbox.network").Info("generation_retired",
				zap.Int64("root_id", rootID), zap.String("reason", "policy transition or stop"))
			delete(o.revoked, rootID)
		}

		if timedOut && !o.stopped {
			o.stopped = true
			close(o.done)
		}

		delete(o.retiring, rootID)
		close(barrier)
		o.mu.Unlock()

		return err
	}
}

func (g *networkGeneration) close(ctx context.Context) error {
	if g == nil {
		return nil
	}

	g.mu.Lock()
	if g.closeDone == nil {
		g.closed = true
		if g.router != nil {
			if err := g.router.Cutoff(); err != nil {
				g.mu.Unlock()
				return err
			}
		}

		g.closeDone = make(chan struct{})
		// Retirement must finish even when the caller stops waiting; otherwise a
		// half-released generation blocks its successor forever.
		go func() { //nolint:contextcheck // Cleanup deliberately outlives the caller's deadline.
			g.closeErr = g.closeResources()
			close(g.closeDone)
		}()
	}

	done := g.closeDone
	g.mu.Unlock()

	select {
	case <-done:
		return g.closeErr
	case <-ctx.Done():
		return fmt.Errorf("wait for network retirement: %w", ctx.Err())
	}
}

func (g *networkGeneration) closeResources() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if g.stopFn != nil {
		return g.stopFn(ctx)
	}

	g.mu.Lock()

	conns := make([]net.Conn, 0, len(g.conns))
	for conn := range g.conns {
		conns = append(conns, conn)
	}
	g.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}

	err := g.router.Stop(ctx)
	_ = g.setup.Close()
	_ = g.link.UserNS.Close()
	_ = g.link.NetNS.Close()
	_ = g.link.BinaryFile.Close()
	closeNetworkConfigFile(g.link.Resolver)
	closeNetworkConfigFile(g.link.Hosts)

	done := make(chan error, 1)
	go func() { done <- g.setup.Cmd.Wait() }()

	select {
	case waitErr := <-done:
		return errors.Join(err, waitErr)
	case <-ctx.Done():
		_ = g.setup.Cmd.Process.Kill()

		<-done

		return errors.Join(err, ctx.Err())
	}
}

func (o *networkOwner) Stop(ctx context.Context) error {
	o.mu.Lock()
	if !o.stopped {
		o.stopped = true
		close(o.done)
	}

	entries := o.entries
	o.entries = make(map[int64]*networkGeneration)

	retiring := make([]chan struct{}, 0, len(o.retiring))
	for _, barrier := range o.retiring {
		retiring = append(retiring, barrier)
	}

	starting := make([]chan struct{}, 0, len(o.starts))
	for _, barrier := range o.starts {
		starting = append(starting, barrier)
	}
	o.mu.Unlock()
	var errs []error

	for _, generation := range entries {
		if err := generation.close(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	for _, barrier := range retiring {
		select {
		case <-barrier:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
	}

	for _, barrier := range starting {
		select {
		case <-barrier:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
	}

	return errors.Join(errs...)
}

// acquirableLocked reports whether rootID can be joined now. A barrier means
// another transition owns the root: wait for it and retry rather than failing.
//
//nolint:nilnil // No barrier and no error is the ordinary "proceed" result.
func (o *networkOwner) acquirableLocked(rootID int64) (<-chan struct{}, error) {
	if o.stopped {
		return nil, errors.New("network owner is stopped")
	}

	if err := o.retireErrors[rootID]; err != nil {
		return nil, fmt.Errorf("previous network retirement failed: %w", err)
	}

	if waiting := o.retiring[rootID]; waiting != nil {
		return waiting, nil
	}

	if o.revoked[rootID] {
		return nil, errors.New("network generation is being revoked")
	}

	return nil, nil
}

// waitBarrier is called with the owner lock released.
func (o *networkOwner) setRetireErrorLocked(rootID int64, err error) {
	if o.retireErrors == nil {
		o.retireErrors = make(map[int64]error)
	}

	o.retireErrors[rootID] = err
}

func (o *networkOwner) reap(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-o.done:
			return
		case <-ticker.C:
			o.reapIdle(ctx, time.Now())
		}
	}
}

// retirement is one generation taken out of service by the idle sweep.
type retirement struct {
	id      int64
	entry   *networkGeneration
	barrier chan struct{}
}

func (o *networkOwner) reapIdle(ctx context.Context, now time.Time) {
	log := logger.Ctx(ctx).Named("sandbox.network")

	for _, item := range o.collectIdle(now, log) {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err := item.entry.close(stopCtx)

		cancel()

		if err != nil {
			log.Error("generation_retire_failed", zap.Int64("root_id", item.id), zap.Error(err))
		} else {
			log.Info("generation_retired", zap.Int64("root_id", item.id), zap.String("reason", "idle"))
		}

		o.mu.Lock()
		if err != nil {
			o.entries[item.id] = item.entry
			o.setRetireErrorLocked(item.id, err)
		}

		delete(o.retiring, item.id)
		close(item.barrier)
		o.mu.Unlock()
	}
}

// collectIdle claims the generations whose idle time has run out and advances
// the idle bookkeeping of the rest. Claiming under the lock is what keeps a
// concurrent acquire from joining a generation already being retired.
func (o *networkOwner) collectIdle(now time.Time, log *zap.Logger) []retirement {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.stopped {
		return nil
	}

	retired := make([]retirement, 0)

	for id, generation := range o.entries {
		if generation.refs > 0 {
			continue
		}

		if o.hasGenerationWorkloads(generation) {
			o.reportBusy(generation, id, log)

			continue
		}

		generation.reportedBusy = false

		if generation.idleSince.IsZero() {
			log.Info("generation_idle", zap.Int64("root_id", id), zap.Duration("retires_in", networkIdleTTL))

			generation.idleSince = now

			continue
		}

		if now.Sub(generation.idleSince) < networkIdleTTL {
			continue
		}

		delete(o.entries, id)

		if o.retiring == nil {
			o.retiring = make(map[int64]chan struct{})
		}

		barrier := make(chan struct{})
		o.retiring[id] = barrier
		retired = append(retired, retirement{id: id, entry: generation, barrier: barrier})
	}

	return retired
}

// reportBusy explains a retention once per busy stretch. The state is reported
// rather than the transition: a generation busy from its first check onwards is
// exactly the case that needs explaining, and it never transitions.
func (o *networkOwner) reportBusy(generation *networkGeneration, id int64, log *zap.Logger) {
	if !generation.reportedBusy {
		log.Info("generation_retained", zap.Int64("root_id", id),
			zap.String("reason", "a process is still running in its namespace"))

		generation.reportedBusy = true
	}

	generation.idleSince = time.Time{}
}

func (o *networkOwner) hasGenerationWorkloads(generation *networkGeneration) bool {
	generation.mu.Lock()
	connected := len(generation.conns) > 0
	generation.mu.Unlock()

	return connected || (o.hasWorkloads != nil && o.hasWorkloads(generation))
}

// namespaceHasWorkloads reports whether a process other than the holder still
// runs in the generation's namespace, which keeps the generation from retiring.
func namespaceHasWorkloads(holderPID int) bool {
	name, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", holderPID))
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}

	self := os.Getpid()

	var opaque []int

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		// The daemon holds ambient capabilities, so it cannot read even its own
		// namespace link; counting itself would retain every generation forever.
		if err != nil || pid == holderPID || pid == self {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
			continue
		}

		other, err := os.Readlink(filepath.Join(string(filepath.Separator), "proc", entry.Name(), "ns", "net"))
		switch {
		case err == nil && strings.EqualFold(other, name):
			return true
		case errors.Is(err, os.ErrPermission):
			opaque = append(opaque, pid)
		}
	}

	return anyDescendant(self, opaque)
}

// anyDescendant reports whether the daemon launched any of these processes. A
// same-user process it cannot inspect — an agent protecting its secrets, or the
// daemon itself — is no evidence of a workload, but one of its own children is.
func anyDescendant(daemon int, candidates []int) bool {
	for _, pid := range candidates {
		if hasAncestor(pid, daemon, processParent) {
			return true
		}
	}

	return false
}

// hasAncestor walks parents until it reaches the ancestor or the tree root. The
// walk is bounded: a reused identifier must not spin it forever.
func hasAncestor(pid, ancestor int, parent func(int) (int, bool)) bool {
	for range maxProcessDepth {
		next, ok := parent(pid)
		if !ok || next <= 0 {
			return false
		}

		if next == ancestor {
			return true
		}

		pid = next
	}

	return false
}

// processParent reads a parent identifier. It stays readable for a process
// whose namespace links are not, which is what makes the ancestry check work.
func processParent(pid int) (int, bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}

	// The command field is parenthesized and may itself contain spaces and
	// parentheses, so the fixed fields begin only after its final bracket.
	tail := bytes.LastIndexByte(raw, ')')
	if tail < 0 {
		return 0, false
	}

	fields := strings.Fields(string(raw[tail+1:]))
	if len(fields) < 2 {
		return 0, false
	}

	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}

	return parent, true
}
