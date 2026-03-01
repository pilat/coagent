package shellenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

// defaultTTL is only a backstop: fingerprint validation is the authoritative
// freshness signal, so this bounds only the un-fingerprintable residue.
const defaultTTL = 30 * time.Minute

// Provider captures and replays per-directory shell activation snapshots. All
// methods degrade gracefully: unavailability is a "" snapshot, never an error a
// caller could propagate into a spawn failure.
type Provider interface {
	// Snapshot ensures a fresh (<TTL) snapshot for workDir and returns its file
	// path, or "" when snapshotting is unavailable (non-bash $SHELL, capture
	// failed/timed out, dir gone). NEVER returns an error — it logs internally
	// and degrades. Callers treat "" as "spawn as today".
	Snapshot(ctx context.Context, workDir string) string

	// Shell is the resolved bash-family shell, or "" if unsupported.
	Shell() string

	// Fingerprint returns a hash of the on-disk state that determines workDir's
	// activated env (toolchain configs, manager state dirs, rc files). Stable
	// while that state is unchanged; changes when it does. "" when unavailable.
	Fingerprint(workDir string) string

	// Invalidate drops any cached snapshot fingerprint for workDir so the next
	// Snapshot recaptures. A best-effort accelerator for env mutations the daemon
	// observes (e.g. a bash command that touched a toolchain manager); never
	// required for correctness — fingerprint validation catches the rest.
	Invalidate(workDir string)

	// WrapExec builds `<shell> -c "source <snap>; exec <argv>"` with Dir=workDir
	// and Env=os.Environ()+extraEnv. With no snapshot it returns a plain exec of
	// argv. Errors only on empty argv, never on snapshot-unavailability.
	WrapExec(ctx context.Context, workDir string, argv, extraEnv []string) (*exec.Cmd, error)

	// Close best-effort removes the per-instance cache dir.
	Close() error
}

var _ Provider = (*provider)(nil)

type provider struct {
	shell      string
	salt       []byte
	instanceID string
	ttl        time.Duration

	// captureFn is the snapshot producer; a field so tests inject a fast fake.
	captureFn func(ctx context.Context, workDir string) ([]byte, error)
	captureN  atomic.Int64

	cacheMu  sync.Mutex
	cacheDir string

	fpMu sync.Mutex
	fps  map[string]string // workDir → fingerprint at last capture

	keyLocks sync.Map // map[string]*sync.Mutex — per-cwd, so first-spawns don't stampede
}

// New constructs a Provider. Infallible: any setup failure yields a provider
// that degrades to no-snapshot (Shell() == ""). The cache dir is created lazily
// in Snapshot.
func New() Provider {
	p := &provider{ttl: defaultTTL}
	p.captureFn = p.capture

	shell := resolveShell()
	if shell == "" {
		return p
	}

	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		logger.Named("shellenv").Warn("salt_generation_failed", zap.Error(err))
		return p
	}

	p.shell = shell
	p.salt = buf[:32]
	p.instanceID = hex.EncodeToString(buf[32:])

	return p
}

func (p *provider) Shell() string { return p.shell }

// Close removes the per-instance cache dir best-effort.
func (p *provider) Close() error {
	p.cacheMu.Lock()
	dir := p.cacheDir
	p.cacheMu.Unlock()

	if dir == "" {
		return nil
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove shellenv cache dir: %w", err)
	}

	return nil
}

func (p *provider) log() *zap.Logger {
	return logger.Named("shellenv")
}

// resolveShell returns the resolved bash-family shell path, or "" when the
// user's shell is not bash. zsh/dash lack `declare -f`/`shopt` compatible dumps;
// supporting them is an explicit follow-up.
func resolveShell() string {
	shell := os.Getenv("SHELL") // nosemgrep: coagent-no-direct-environment-read -- $SHELL is not a coagent secret
	if shell == "" {
		found, err := exec.LookPath("bash")
		if err != nil {
			return ""
		}

		shell = found
	}

	resolved, err := filepath.EvalSymlinks(shell)
	if err != nil {
		resolved = shell
	}

	if filepath.Base(resolved) != "bash" {
		return ""
	}

	return resolved
}
