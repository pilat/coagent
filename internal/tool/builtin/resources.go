package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/shellenv"
	"github.com/pilat/coagent/internal/tool"
)

// Resources retains isolated session resources between tool-stack activations.
type Resources interface {
	tool.ResourceLifecycle
	acquire(StackConfig, bashsandbox.Config) (*resourceLease, error)
}

var _ Resources = (*resources)(nil)

type resources struct {
	mu      sync.Mutex
	entries map[int64]*resourceLease
	closed  bool
}

type resourceLease struct {
	owner       *resources
	sessionID   int64
	projectID   int64
	key         string
	busy        bool
	retired     bool
	provider    shellenv.Provider
	ownProvider bool
	mcp         mcp.Service
	snapshot    os.FileInfo
}

// NewResources creates an owner that must close after all session runners join.
func NewResources() Resources {
	return &resources{entries: make(map[int64]*resourceLease)}
}

func (r *resources) Retire(sessionID int64) error {
	r.mu.Lock()
	entry := r.entries[sessionID]

	closeNow := entry != nil && !entry.busy
	if closeNow {
		delete(r.entries, sessionID)
	}

	if entry != nil {
		entry.retired = true
	}
	r.mu.Unlock()

	if closeNow {
		return entry.close()
	}

	return nil
}

func (r *resources) Invalidate(projectID int64) error {
	r.mu.Lock()
	var idle []*resourceLease

	for id, entry := range r.entries {
		if projectID != 0 && entry.projectID != projectID {
			continue
		}

		entry.retired = true
		if !entry.busy {
			idle = append(idle, entry)

			delete(r.entries, id)
		}
	}
	r.mu.Unlock()

	var err error
	for _, entry := range idle {
		err = errors.Join(err, entry.close())
	}

	return err
}

func (r *resources) Close() error {
	r.mu.Lock()
	r.closed = true
	var idle []*resourceLease

	for id, entry := range r.entries {
		entry.retired = true
		if !entry.busy {
			idle = append(idle, entry)

			delete(r.entries, id)
		}
	}
	r.mu.Unlock()

	var err error
	for _, entry := range idle {
		err = errors.Join(err, entry.close())
	}

	return err
}

func (r *resources) acquire(cfg StackConfig, policy bashsandbox.Config) (*resourceLease, error) {
	key, err := resourceKey(cfg, policy)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("session tool resources are closed")
	}

	old := r.entries[cfg.SessionID]
	if old != nil && old.busy {
		r.mu.Unlock()
		return nil, fmt.Errorf("session %d already owns an active tool stack", cfg.SessionID)
	}

	if old != nil && !old.retired && old.key == key {
		old.busy = true
		r.mu.Unlock()

		return old, nil
	}

	entry := newResourceLease(cfg)
	entry.owner, entry.key = r, key
	r.entries[cfg.SessionID] = entry
	r.mu.Unlock()

	if old != nil {
		if err := old.close(); err != nil {
			_ = r.Retire(cfg.SessionID)
			_ = entry.release()

			return nil, err
		}
	}

	return entry, nil
}

func newResourceLease(cfg StackConfig) *resourceLease {
	provider, owned := stackProvider(cfg)

	return &resourceLease{
		sessionID:   cfg.SessionID,
		projectID:   cfg.ProjectID,
		busy:        true,
		provider:    provider,
		ownProvider: owned,
	}
}

func resourceKey(cfg StackConfig, policy bashsandbox.Config) (string, error) {
	encoded, err := json.Marshal(struct {
		WorkDir   string                      `json:"work_dir"`
		ProjectID int64                       `json:"project_id"`
		Enabled   bool                        `json:"enabled"`
		Policy    string                      `json:"policy"`
		Servers   map[string]mcp.ServerConfig `json:"servers"`
	}{cfg.WorkDir, cfg.ProjectID, policy.Enabled, policy.Policy.Digest, cfg.Servers})
	if err != nil {
		return "", fmt.Errorf("encode tool resource identity: %w", err)
	}

	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}

func (e *resourceLease) release() error {
	if e.owner == nil {
		return e.close()
	}

	e.owner.mu.Lock()
	e.busy = false

	retired := e.retired
	if retired && e.owner.entries[e.sessionID] == e {
		delete(e.owner.entries, e.sessionID)
	}
	e.owner.mu.Unlock()

	if retired {
		return e.close()
	}

	return nil
}

func (e *resourceLease) close() error {
	if e.mcp != nil {
		e.mcp.Stop()
	}

	if e.ownProvider {
		if err := e.provider.Close(); err != nil {
			return fmt.Errorf("close shell provider: %w", err)
		}
	}

	return nil
}

func (e *resourceLease) acquireMCP(
	ctx context.Context,
	cfg StackConfig,
	runner bashsandbox.Runner,
) (mcp.Service, error) {
	var snapshot os.FileInfo

	if len(cfg.Servers) > 0 {
		confined, _ := runner.(shellenv.ConfinedRunner)

		path := e.provider.Snapshot(ctx, confined, cfg.WorkDir)
		if path != "" {
			snapshot, _ = os.Stat(path)
		}
	}

	unchanged := snapshot == nil && e.snapshot == nil ||
		snapshot != nil && e.snapshot != nil && os.SameFile(snapshot, e.snapshot) &&
			snapshot.ModTime().Equal(e.snapshot.ModTime()) && snapshot.Size() == e.snapshot.Size()
	if e.mcp != nil && unchanged && e.mcp.Refresh(ctx) {
		return e.mcp, nil
	}

	if e.mcp != nil {
		e.mcp.Stop()
		e.mcp = nil
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire MCP resources: %w", err)
	}

	processRunner := procexec.Runner(runner)
	service, err := mcp.AcquireForWorkDir(ctx, cfg.Servers, cfg.WorkDir, e.provider, processRunner)

	e.mcp, e.snapshot = service, snapshot
	if err != nil {
		return service, fmt.Errorf("acquire MCP resources: %w", err)
	}

	return service, nil
}
